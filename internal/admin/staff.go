package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// Akun admin (§5.29). Hanya super admin. Admin outlet login dengan email & password
// lewat POST /v1/auth/login yang sama; aksesnya dicek ulang setiap request.

type StaffDTO struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	StoreID   *string `json:"store_id"`
	StoreName *string `json:"store_name"`
	CreatedAt string  `json:"created_at"`
}

const staffSelect = `SELECT u.id::text, u.name, u.email, u.role, u.store_id, s.name, u.created_at
	FROM users u LEFT JOIN stores s ON s.id = u.store_id `

func scanStaff(row pgx.CollectableRow) (StaffDTO, error) {
	var d StaffDTO
	var at time.Time
	err := row.Scan(&d.ID, &d.Name, &d.Email, &d.Role, &d.StoreID, &d.StoreName, &at)
	d.CreatedAt = httpx.ISO(at)
	return d, err
}

func (h *Handler) staff(ctx context.Context, id string) (StaffDTO, error) {
	rows, err := h.pool.Query(ctx, staffSelect+`WHERE u.id::text = $1`, id)
	if err != nil {
		return StaffDTO{}, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, scanStaff)
	if database.IsNoRows(err) {
		return d, httpx.NotFound("Akun tidak ditemukan.")
	}
	return d, err
}

// GET /v1/admin/staff — semua super admin & admin outlet.
func (h *Handler) Staff(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), staffSelect+`
		WHERE u.role IN ('SUPER_ADMIN', 'OUTLET_ADMIN') ORDER BY u.role DESC, s.sort_order NULLS FIRST, u.name`)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, scanStaff)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

type staffRequest struct {
	Name     *string `json:"name" validate:"omitempty,max=100"`
	Email    *string `json:"email" validate:"omitempty,email,max=254"`
	Password *string `json:"password" validate:"omitempty,min=8,max=72"`
	// Role: SUPER_ADMIN | OUTLET_ADMIN; PATCH dengan CUSTOMER = cabut akses admin.
	Role    *string `json:"role" validate:"omitempty,oneof=SUPER_ADMIN OUTLET_ADMIN CUSTOMER"`
	StoreID *string `json:"store_id" validate:"omitempty,max=50"`
}

// roleStore: store_id yang disimpan untuk role (wajib & valid untuk admin outlet, null selain itu).
func (h *Handler) roleStore(ctx context.Context, role, storeID string) (*string, error) {
	if role != RoleOutletAdmin {
		return nil, nil
	}
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return nil, httpx.Validation("Pilih outlet untuk admin outlet.", httpx.FieldDetail{Field: "store_id", Message: "wajib diisi"})
	}
	if _, ok, err := h.catalog.StoreByID(ctx, storeID, false); err != nil || !ok {
		if err != nil {
			return nil, err
		}
		return nil, httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
	}
	return &storeID, nil
}

// POST /v1/admin/staff { name, email, password, role, store_id } → 201
func (h *Handler) CreateStaff(w http.ResponseWriter, r *http.Request) {
	var req staffRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	name, email, role := strings.TrimSpace(deref(req.Name)), strings.TrimSpace(deref(req.Email)), deref(req.Role)
	if role == "" {
		role = RoleOutletAdmin
	}
	var missing []httpx.FieldDetail
	for field, v := range map[string]string{"name": name, "email": email, "password": deref(req.Password)} {
		if v == "" {
			missing = append(missing, httpx.FieldDetail{Field: field, Message: "wajib diisi"})
		}
	}
	if role == "CUSTOMER" {
		missing = append(missing, httpx.FieldDetail{Field: "role", Message: "harus SUPER_ADMIN atau OUTLET_ADMIN"})
	}
	if len(missing) > 0 {
		httpx.FromError(w, r, httpx.Validation("Data akun belum lengkap.", missing...))
		return
	}
	ctx := r.Context()
	storeID, err := h.roleStore(ctx, role, deref(req.StoreID))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var id string
	err = h.pool.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, role, store_id) VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text`, name, email, string(hash), role, storeID).Scan(&id)
	if database.IsUniqueViolation(err) {
		err = httpx.NewError(http.StatusConflict, "EMAIL_ALREADY_EXISTS", "Email ini sudah terdaftar.")
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	d, err := h.staff(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "STAFF_CREATE", "user", id, fmt.Sprintf("Akun %s (%s) dibuat", d.Name, staffLabel(d)), d)
	httpx.JSONMessage(w, http.StatusCreated, "Akun admin dibuat", d)
}

// PATCH /v1/admin/staff/{id} { name, password, role, store_id } — role CUSTOMER = cabut akses.
func (h *Handler) UpdateStaff(w http.ResponseWriter, r *http.Request) {
	var req staffRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == adminFrom(ctx).ID && req.Role != nil {
		httpx.FromError(w, r, httpx.Validation("Kamu tidak bisa mengubah role akunmu sendiri."))
		return
	}
	cur, err := h.staff(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	role := cur.Role
	if req.Role != nil {
		role = *req.Role
	}
	storeIn := deref(cur.StoreID)
	if req.StoreID != nil {
		storeIn = *req.StoreID
	}
	storeID, err := h.roleStore(ctx, role, storeIn)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var hash *string
	if req.Password != nil {
		b, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			httpx.FromError(w, r, err)
			return
		}
		s := string(b)
		hash = &s
	}
	var name *string
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		name = &n
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE users SET name = COALESCE(NULLIF($2, ''), name), password_hash = COALESCE($3, password_hash),
		       role = $4, store_id = $5, updated_at = now()
		WHERE id::text = $1`, id, name, hash, role, storeID); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	d, err := h.staff(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	summary := fmt.Sprintf("Akun %s diperbarui (%s)", d.Name, staffLabel(d))
	if role == "CUSTOMER" {
		summary = fmt.Sprintf("Akses admin %s dicabut", d.Name)
	}
	h.audit(r, "STAFF_UPDATE", "user", id, summary, d)
	httpx.JSONMessage(w, http.StatusOK, "Akun admin diperbarui", d)
}

func staffLabel(d StaffDTO) string {
	if d.Role == RoleOutletAdmin && d.StoreName != nil {
		return "admin outlet " + *d.StoreName
	}
	if d.Role == RoleSuperAdmin {
		return "super admin"
	}
	return "pelanggan"
}
