package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// Kategori menu (§5.29). Semua admin bisa membaca; tambah & ubah hanya super admin.
// Kategori tidak dihapus (menu lama masih merujuknya); nonaktifkan untuk menyembunyikan dari app.

type AdminCategoryDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Icon         string `json:"icon"`
	SortOrder    int    `json:"sort_order"`
	IsActive     bool   `json:"is_active"`
	ProductCount int    `json:"product_count"`
}

const categorySelect = `SELECT c.id, c.name, c.icon, c.sort_order, c.is_active,
	(SELECT count(*)::int FROM products p WHERE p.category_id = c.id) FROM categories c `

func scanCategory(row pgx.CollectableRow) (AdminCategoryDTO, error) {
	var c AdminCategoryDTO
	err := row.Scan(&c.ID, &c.Name, &c.Icon, &c.SortOrder, &c.IsActive, &c.ProductCount)
	return c, err
}

func (h *Handler) category(ctx context.Context, id string) (AdminCategoryDTO, error) {
	rows, err := h.pool.Query(ctx, categorySelect+`WHERE c.id = $1`, id)
	if err != nil {
		return AdminCategoryDTO{}, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanCategory)
	if database.IsNoRows(err) {
		return c, httpx.NotFound("Kategori tidak ditemukan.")
	}
	return c, err
}

// GET /v1/admin/categories — termasuk yang nonaktif.
func (h *Handler) Categories(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), categorySelect+`ORDER BY c.sort_order, c.id`)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, scanCategory)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

type categoryRequest struct {
	ID        *string `json:"id" validate:"omitempty,max=40"` // kosong = dibuat dari nama, mis. "Kue & Roti" → "kue-roti"
	Name      *string `json:"name" validate:"omitempty,max=50"`
	Icon      *string `json:"icon" validate:"omitempty,max=16"`
	SortOrder *int    `json:"sort_order" validate:"omitempty,min=0,max=1000"`
	IsActive  *bool   `json:"is_active"`
}

// POST /v1/admin/categories → 201. Wajib: name.
func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	if name == "" {
		httpx.FromError(w, r, httpx.Validation("Data kategori belum lengkap.", httpx.FieldDetail{Field: "name", Message: "wajib diisi"}))
		return
	}
	id := slugify(strings.TrimSpace(deref(req.ID)))
	if strings.TrimSpace(deref(req.ID)) == "" {
		id = slugify(name)
	}
	icon := strings.TrimSpace(deref(req.Icon))
	if icon == "" {
		icon = "🍽️"
	}
	ctx := r.Context()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO categories (id, name, icon, sort_order, is_active)
		VALUES ($1, $2, $3, COALESCE($4, (SELECT COALESCE(max(sort_order), 0) + 1 FROM categories)), COALESCE($5, TRUE))`,
		id, name, icon, req.SortOrder, req.IsActive)
	if database.IsUniqueViolation(err) {
		err = httpx.NewError(http.StatusConflict, "CATEGORY_EXISTS", fmt.Sprintf("Kategori dengan id %q sudah ada.", id))
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	c, err := h.category(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "CATEGORY_CREATE", "category", id, fmt.Sprintf("Kategori baru %s %s", c.Icon, c.Name), c)
	httpx.JSONMessage(w, http.StatusCreated, "Kategori ditambahkan", c)
}

// PATCH /v1/admin/categories/{id}
func (h *Handler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		httpx.FromError(w, r, httpx.Validation("Data kategori belum lengkap.", httpx.FieldDetail{Field: "name", Message: "wajib diisi"}))
		return
	}
	trim := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := strings.TrimSpace(*p)
		return &v
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	tag, err := h.pool.Exec(ctx, `
		UPDATE categories SET name = COALESCE($2, name), icon = COALESCE(NULLIF($3, ''), icon),
		       sort_order = COALESCE($4, sort_order), is_active = COALESCE($5, is_active)
		WHERE id = $1`, id, trim(req.Name), trim(req.Icon), req.SortOrder, req.IsActive)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.NotFound("Kategori tidak ditemukan.")
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	c, err := h.category(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	summary := "Kategori %s diperbarui"
	if req.IsActive != nil {
		summary = map[bool]string{true: "Kategori %s diaktifkan", false: "Kategori %s disembunyikan"}[*req.IsActive]
	}
	h.audit(r, "CATEGORY_UPDATE", "category", id, fmt.Sprintf(summary, c.Name), c)
	httpx.JSONMessage(w, http.StatusOK, "Kategori diperbarui", c)
}
