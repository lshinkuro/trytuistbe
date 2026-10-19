package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// Web admin → Banner (super admin): banner Beranda + aksi saat di-tap.

type AdminBannerDTO struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Subtitle    string  `json:"subtitle"`
	ImageURL    string  `json:"image_url"`
	ActionType  string  `json:"action_type"`
	ActionValue *string `json:"action_value"`
	// Nama produk / kategori tujuan (null bila tidak ada / sudah dihapus).
	ActionLabel *string `json:"action_label"`
	StartsAt    *string `json:"starts_at"`
	EndsAt      *string `json:"ends_at"`
	SortOrder   int     `json:"sort_order"`
	IsActive    bool    `json:"is_active"`
	// LIVE (tampil di app) | SCHEDULED | ENDED | INACTIVE | HIDDEN (tujuan tidak berlaku, mis. kupon dimatikan)
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

const adminBannerSelect = `
	SELECT b.id, b.title, b.subtitle, b.image_url, b.action_type, b.action_value,
	       CASE b.action_type WHEN 'PRODUCT' THEN (SELECT name FROM products WHERE id = b.action_value)
	                          WHEN 'CATEGORY' THEN (SELECT name FROM categories WHERE id = b.action_value) END,
	       b.starts_at, b.ends_at, b.sort_order, b.is_active, b.updated_at
	FROM banners b `

func scanAdminBanner(row pgx.CollectableRow) (AdminBannerDTO, error) {
	var d AdminBannerDTO
	var starts, ends *time.Time
	var updated time.Time
	err := row.Scan(&d.ID, &d.Title, &d.Subtitle, &d.ImageURL, &d.ActionType, &d.ActionValue, &d.ActionLabel,
		&starts, &ends, &d.SortOrder, &d.IsActive, &updated)
	d.StartsAt, d.EndsAt, d.UpdatedAt = httpx.ISOPtr(starts), httpx.ISOPtr(ends), httpx.ISO(updated)
	now := time.Now()
	switch {
	case !d.IsActive:
		d.Status = "INACTIVE"
	case starts != nil && starts.After(now):
		d.Status = "SCHEDULED"
	case ends != nil && ends.Before(now):
		d.Status = "ENDED"
	default:
		d.Status = "LIVE"
	}
	return d, err
}

// markHidden: banner yang dalam periode tapi disembunyikan dari app (aturan sama dengan catalog.ActiveBanners).
func (h *Handler) markHidden(ctx context.Context, list []AdminBannerDTO) error {
	live, err := catalog.NewRepository(h.pool).ActiveBanners(ctx)
	if err != nil {
		return err
	}
	shown := map[string]bool{}
	for _, b := range live {
		shown[b.ID] = true
	}
	for i := range list {
		if list[i].Status == "LIVE" && !shown[list[i].ID] {
			list[i].Status = "HIDDEN"
		}
	}
	return nil
}

func (h *Handler) banner(ctx context.Context, id string) (AdminBannerDTO, error) {
	rows, err := h.pool.Query(ctx, adminBannerSelect+"WHERE b.id = $1", id)
	if err != nil {
		return AdminBannerDTO{}, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanAdminBanner)
	if database.IsNoRows(err) {
		return b, httpx.NotFound("Banner tidak ditemukan.")
	}
	if err != nil {
		return b, err
	}
	list := []AdminBannerDTO{b}
	err = h.markHidden(ctx, list)
	return list[0], err
}

// GET /v1/admin/banners — semua banner, urutan tampil.
func (h *Handler) Banners(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), adminBannerSelect+"ORDER BY b.sort_order, b.id")
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, scanAdminBanner)
	if err == nil {
		err = h.markHidden(r.Context(), list)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

type bannerRequest struct {
	Title       *string    `json:"title" validate:"omitempty,max=80"`
	Subtitle    *string    `json:"subtitle" validate:"omitempty,max=120"`
	ImageURL    *string    `json:"image_url" validate:"omitempty,max=1000"`
	ActionType  *string    `json:"action_type" validate:"omitempty,oneof=NONE PRODUCT CATEGORY FREE_DELIVERY"`
	ActionValue *string    `json:"action_value" validate:"omitempty,max=50"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	// Kirim true untuk mengosongkan periode (tampil tanpa batas waktu).
	ClearSchedule bool  `json:"clear_schedule"`
	SortOrder     *int  `json:"sort_order" validate:"omitempty,min=0,max=1000"`
	IsActive      *bool `json:"is_active"`
}

// resolved: nilai akhir banner setelah request diterapkan ke data lama.
type bannerValues struct {
	Title, Subtitle, ImageURL, ActionType string
	ActionValue                           *string
	StartsAt, EndsAt                      *time.Time
	SortOrder                             int
	IsActive                              bool
}

func (req bannerRequest) apply(v bannerValues) bannerValues {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&v.Title, req.Title)
	set(&v.Subtitle, req.Subtitle)
	set(&v.ImageURL, req.ImageURL)
	set(&v.ActionType, req.ActionType)
	if req.ActionValue != nil {
		val := strings.TrimSpace(*req.ActionValue)
		v.ActionValue = &val
	}
	if v.ActionType == catalog.BannerNone || v.ActionType == catalog.BannerFreeDelivery {
		v.ActionValue = nil
	}
	if req.ClearSchedule {
		v.StartsAt, v.EndsAt = nil, nil
	}
	if req.StartsAt != nil {
		v.StartsAt = req.StartsAt
	}
	if req.EndsAt != nil {
		v.EndsAt = req.EndsAt
	}
	if req.SortOrder != nil {
		v.SortOrder = *req.SortOrder
	}
	if req.IsActive != nil {
		v.IsActive = *req.IsActive
	}
	return v
}

func (v bannerValues) validate(ctx context.Context, db database.DBTX) error {
	var d []httpx.FieldDetail
	if v.Title == "" {
		d = append(d, httpx.FieldDetail{Field: "title", Message: "wajib diisi"})
	}
	if !strings.HasPrefix(v.ImageURL, "https://") {
		d = append(d, httpx.FieldDetail{Field: "image_url", Message: "wajib diisi, diawali https://"})
	}
	if v.StartsAt != nil && v.EndsAt != nil && !v.EndsAt.After(*v.StartsAt) {
		d = append(d, httpx.FieldDetail{Field: "ends_at", Message: "harus setelah waktu mulai"})
	}
	switch v.ActionType {
	case catalog.BannerProduct, catalog.BannerCategory:
		table, label := "products", "menu"
		if v.ActionType == catalog.BannerCategory {
			table, label = "categories", "kategori"
		}
		var ok bool
		if v.ActionValue != nil && *v.ActionValue != "" {
			if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, *v.ActionValue).Scan(&ok); err != nil {
				return err
			}
		}
		if !ok {
			d = append(d, httpx.FieldDetail{Field: "action_value", Message: "pilih " + label + " tujuan"})
		}
	}
	if len(d) > 0 {
		return httpx.Validation("Data banner belum lengkap.", d...)
	}
	return nil
}

func newBannerID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "BN-" + strings.ToUpper(hex.EncodeToString(b)), nil
}

// POST /v1/admin/banners → 201. Wajib: title, image_url; aksi default NONE.
func (h *Handler) CreateBanner(w http.ResponseWriter, r *http.Request) {
	var req bannerRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	v := req.apply(bannerValues{ActionType: catalog.BannerNone, IsActive: true})
	if err := v.validate(ctx, h.pool); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	id, err := newBannerID()
	if err == nil {
		_, err = h.pool.Exec(ctx, `
			INSERT INTO banners (id, title, subtitle, image_url, action_type, action_value, starts_at, ends_at, sort_order, is_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, v.Title, v.Subtitle, v.ImageURL, v.ActionType, v.ActionValue, v.StartsAt, v.EndsAt, v.SortOrder, v.IsActive)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	b, err := h.banner(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "BANNER_CREATE", "banner", id, "Banner baru: "+b.Title, b)
	httpx.JSONMessage(w, http.StatusCreated, "Banner dibuat", b)
}

// PATCH /v1/admin/banners/{id}
func (h *Handler) UpdateBanner(w http.ResponseWriter, r *http.Request) {
	var req bannerRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var before AdminBannerDTO
	err := database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		var cur bannerValues
		err := tx.QueryRow(ctx, `SELECT title, subtitle, image_url, action_type, action_value, starts_at, ends_at, sort_order, is_active
			FROM banners WHERE id = $1 FOR UPDATE`, id).
			Scan(&cur.Title, &cur.Subtitle, &cur.ImageURL, &cur.ActionType, &cur.ActionValue, &cur.StartsAt, &cur.EndsAt, &cur.SortOrder, &cur.IsActive)
		if database.IsNoRows(err) {
			return httpx.NotFound("Banner tidak ditemukan.")
		}
		if err != nil {
			return err
		}
		before = AdminBannerDTO{Title: cur.Title, ActionType: cur.ActionType, ActionValue: cur.ActionValue, IsActive: cur.IsActive}
		v := req.apply(cur)
		if err := v.validate(ctx, tx); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE banners SET title = $2, subtitle = $3, image_url = $4, action_type = $5, action_value = $6,
				starts_at = $7, ends_at = $8, sort_order = $9, is_active = $10, updated_at = now()
			WHERE id = $1`, id, v.Title, v.Subtitle, v.ImageURL, v.ActionType, v.ActionValue, v.StartsAt, v.EndsAt, v.SortOrder, v.IsActive)
		return err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	after, err := h.banner(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "BANNER_UPDATE", "banner", id, fmt.Sprintf("Banner %q diperbarui", after.Title),
		map[string]any{"before": before, "after": after})
	httpx.JSONMessage(w, http.StatusOK, "Banner diperbarui", after)
}

// DELETE /v1/admin/banners/{id}
func (h *Handler) DeleteBanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	b, err := h.banner(ctx, id)
	if err == nil {
		_, err = h.pool.Exec(ctx, `DELETE FROM banners WHERE id = $1`, id)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "BANNER_DELETE", "banner", id, "Banner dihapus: "+b.Title, b)
	httpx.JSONMessage(w, http.StatusOK, "Banner dihapus", nil)
}
