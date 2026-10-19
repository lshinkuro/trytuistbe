// Package admin: API untuk web admin trytuistadmin (§5.25).
//
// Login memakai POST /v1/auth/login yang sama dengan aplikasi. Semua route
// /v1/admin/* memakai Auth + RequireAdmin: role dicek ke DB setiap request,
// jadi mencabut role admin langsung berlaku tanpa menunggu token habis.
//
// Dua role (§5.29): SUPER_ADMIN melihat & mengatur semua outlet; OUTLET_ADMIN terikat ke
// satu outlet (users.store_id) dan semua data yang dibaca/diubah dipaksa ke outlet itu.
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
	"trytuist-api/internal/payment"
)

const (
	RoleSuperAdmin  = "SUPER_ADMIN"
	RoleOutletAdmin = "OUTLET_ADMIN"
)

type Admin struct {
	ID        string
	Name      string
	Email     string
	Role      string
	StoreID   string // hanya OUTLET_ADMIN
	StoreName string
}

func (a Admin) IsSuper() bool { return a.Role == RoleSuperAdmin }

type ctxKey struct{}

func adminFrom(ctx context.Context) Admin {
	a, _ := ctx.Value(ctxKey{}).(Admin)
	return a
}

var (
	errNotAdmin   = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "Akun ini tidak punya akses admin.")
	errNotSuper   = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "Hanya super admin yang bisa melakukan ini.")
	errOtherStore = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "Kamu hanya bisa mengatur outletmu sendiri.")
)

// scopeStore: outlet yang dipakai sebuah request. Admin outlet selalu outletnya sendiri
// (meminta outlet lain → 403); super admin memakai requested (” = semua outlet).
func scopeStore(ctx context.Context, requested string) (string, error) {
	a := adminFrom(ctx)
	requested = strings.TrimSpace(requested)
	if a.IsSuper() {
		return requested, nil
	}
	if requested != "" && requested != a.StoreID {
		return "", errOtherStore
	}
	return a.StoreID, nil
}

// canManageStore: super admin, atau admin outlet untuk outletnya sendiri.
func canManageStore(ctx context.Context, storeID string) error {
	if a := adminFrom(ctx); !a.IsSuper() && a.StoreID != storeID {
		return errOtherStore
	}
	return nil
}

// RequireSuperAdmin dipasang setelah RequireAdmin untuk route khusus pusat.
func RequireSuperAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !adminFrom(r.Context()).IsSuper() {
			httpx.FromError(w, r, errNotSuper)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin dipasang setelah httpx.Auth.
func RequireAdmin(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			var a Admin
			err := pool.QueryRow(ctx, `
				SELECT u.id::text, u.name, u.email, u.role, COALESCE(u.store_id, ''), COALESCE(s.name, '')
				FROM users u LEFT JOIN stores s ON s.id = u.store_id WHERE u.id = $1`, httpx.UserID(ctx)).
				Scan(&a.ID, &a.Name, &a.Email, &a.Role, &a.StoreID, &a.StoreName)
			if database.IsNoRows(err) || (err == nil && a.Role != RoleSuperAdmin && a.Role != RoleOutletAdmin) {
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
	pool      *pgxpool.Pool
	orders    *order.Service
	payments  *payment.Service
	appConfig *appconfig.Repository
	catalog   *catalog.Repository
	loc       *time.Location
	now       func() time.Time
}

func NewHandler(pool *pgxpool.Pool, orders *order.Service, payments *payment.Service, appConfig *appconfig.Repository, loc *time.Location) *Handler {
	if loc == nil {
		loc = httpx.Loc
	}
	return &Handler{pool: pool, orders: orders, payments: payments, appConfig: appConfig,
		catalog: catalog.NewRepository(pool), loc: loc, now: time.Now}
}

type MeDTO struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	StoreID   *string `json:"store_id"` // null untuk super admin
	StoreName *string `json:"store_name"`
}

// GET /v1/admin/me — dipakai web admin setelah login untuk memastikan akun ini admin & membaca role-nya.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r.Context())
	me := MeDTO{ID: a.ID, Name: a.Name, Email: a.Email, Role: a.Role}
	if a.StoreID != "" {
		me.StoreID, me.StoreName = &a.StoreID, &a.StoreName
	}
	httpx.JSON(w, http.StatusOK, me)
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
