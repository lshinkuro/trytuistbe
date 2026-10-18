// Package admin: API untuk web admin trytuistadmin (§5.25).
//
// Login memakai POST /v1/auth/login yang sama dengan aplikasi. Semua route
// /v1/admin/* memakai Auth + RequireAdmin: role dicek ke DB setiap request,
// jadi mencabut role ADMIN langsung berlaku tanpa menunggu token habis.
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
)

const RoleAdmin = "ADMIN"

type Admin struct {
	ID    string
	Name  string
	Email string
}

type ctxKey struct{}

func adminFrom(ctx context.Context) Admin {
	a, _ := ctx.Value(ctxKey{}).(Admin)
	return a
}

var errNotAdmin = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "Akun ini tidak punya akses admin.")

// RequireAdmin dipasang setelah httpx.Auth.
func RequireAdmin(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			var a Admin
			var role string
			err := pool.QueryRow(ctx, `SELECT id::text, name, email, role FROM users WHERE id = $1`, httpx.UserID(ctx)).
				Scan(&a.ID, &a.Name, &a.Email, &role)
			if database.IsNoRows(err) || (err == nil && role != RoleAdmin) {
				httpx.FromError(w, r, errNotAdmin)
				return
			}
			if err != nil {
				httpx.FromError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKey{}, a)))
		})
	}
}

type Handler struct {
	pool    *pgxpool.Pool
	orders  *order.Service
	catalog *catalog.Repository
	loc     *time.Location
	now     func() time.Time
}

func NewHandler(pool *pgxpool.Pool, orders *order.Service, loc *time.Location) *Handler {
	if loc == nil {
		loc = httpx.Loc
	}
	return &Handler{pool: pool, orders: orders, catalog: catalog.NewRepository(pool), loc: loc, now: time.Now}
}

// GET /v1/admin/me — dipakai web admin setelah login untuk memastikan akun ini admin.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r.Context())
	httpx.JSON(w, http.StatusOK, map[string]string{"id": a.ID, "name": a.Name, "email": a.Email, "role": RoleAdmin})
}

// ===== Audit log =====

func writeAudit(ctx context.Context, db database.DBTX, adminID, action, entity, entityID, summary string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO admin_audit_logs (admin_id, action, entity, entity_id, summary, data)
		VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6)`, adminID, action, entity, entityID, summary, b)
	return err
}

// audit di luar transaksi: gagal mencatat tidak membatalkan aksi yang sudah berhasil.
func (h *Handler) audit(r *http.Request, action, entity, entityID, summary string, data any) {
	ctx := r.Context()
	if err := writeAudit(ctx, h.pool, adminFrom(ctx).ID, action, entity, entityID, summary, data); err != nil {
		slog.ErrorContext(ctx, "audit log", "err", err, "action", action, "entity_id", entityID)
	}
}

type AuditLogDTO struct {
	ID        string          `json:"id"`
	Action    string          `json:"action"`
	Entity    string          `json:"entity"`
	EntityID  string          `json:"entity_id"`
	Summary   string          `json:"summary"`
	Data      json.RawMessage `json:"data"`
	AdminName *string         `json:"admin_name"`
	CreatedAt string          `json:"created_at"`
}

// GET /v1/admin/audit-logs?page=&limit=
func (h *Handler) AuditLogs(w http.ResponseWriter, r *http.Request) {
	page, err := httpx.ParsePage(r.URL.Query(), 20)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	var total int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_logs`).Scan(&total); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT l.id::text, l.action, l.entity, l.entity_id, l.summary, l.data, u.name, l.created_at
		FROM admin_audit_logs l LEFT JOIN users u ON u.id = l.admin_id
		ORDER BY l.created_at DESC, l.id LIMIT $1 OFFSET $2`, page.Limit, page.Offset())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditLogDTO, error) {
		var d AuditLogDTO
		var at time.Time
		err := row.Scan(&d.ID, &d.Action, &d.Entity, &d.EntityID, &d.Summary, &d.Data, &d.AdminName, &at)
		d.CreatedAt = httpx.ISO(at)
		return d, err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list, page.Meta(total))
}
