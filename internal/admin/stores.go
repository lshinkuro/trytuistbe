package admin

import (
	"context"
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

// Outlet di web admin (§5.26).

type StoreTodayDTO struct {
	Orders  int   `json:"orders"`
	Revenue int64 `json:"revenue"` // pesanan sukses hari ini
	Active  int   `json:"active"`  // pesanan yang masih berjalan
}

type AdminStoreDTO struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Address          string        `json:"address"`
	Lat              float64       `json:"lat"`
	Lng              float64       `json:"lng"`
	Phone            *string       `json:"phone"`
	OpensAt          string        `json:"opens_at"`
	ClosesAt         string        `json:"closes_at"`
	HoursLabel       string        `json:"hours_label"`
	RadiusKM         float64       `json:"delivery_radius_km"`
	IsActive         bool          `json:"is_active"`
	IsTempClosed     bool          `json:"is_temporarily_closed"` // tombol tutup sementara
	IsOpenNow        bool          `json:"is_open_now"`
	SortOrder        int           `json:"sort_order"`
	UnavailableCount int           `json:"unavailable_products"` // menu yang ditandai habis khusus outlet ini
	Today            StoreTodayDTO `json:"today"`
}

const storeIDLockKey = 7_250_002

func (h *Handler) storeDTO(ctx context.Context, s catalog.Store) (AdminStoreDTO, error) {
	d := AdminStoreDTO{
		ID: s.ID, Name: s.Name, Address: s.Address, Lat: s.Lat, Lng: s.Lng, Phone: s.Phone,
		OpensAt: catalog.FormatClock(s.OpensAt), ClosesAt: catalog.FormatClock(s.ClosesAt), HoursLabel: s.HoursLabel(),
		RadiusKM: s.RadiusKM, IsActive: s.IsActive, IsTempClosed: !s.IsOpen, IsOpenNow: s.IsOpenAt(h.now(), h.loc),
		SortOrder: s.Sort,
	}
	now := h.now().In(h.loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, h.loc)
	err := h.pool.QueryRow(ctx, `
		SELECT count(*)::int,
		       COALESCE(sum(total) FILTER (WHERE status = 'COMPLETED'), 0)::bigint,
		       count(*) FILTER (WHERE status IN ('PENDING','PAID','PROCESSING','DELIVERING'))::int,
		       (SELECT count(*)::int FROM store_product_availability WHERE store_id = $1 AND NOT is_available)
		FROM orders WHERE store_id = $1 AND created_at >= $2`, s.ID, dayStart).
		Scan(&d.Today.Orders, &d.Today.Revenue, &d.Today.Active, &d.UnavailableCount)
	return d, err
}

// GET /v1/admin/stores — semua outlet (termasuk nonaktif) + ringkasan hari ini.
func (h *Handler) Stores(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stores, err := h.catalog.AllStores(ctx)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := make([]AdminStoreDTO, 0, len(stores))
	for _, s := range stores {
		d, err := h.storeDTO(ctx, s)
		if err != nil {
			httpx.FromError(w, r, err)
			return
		}
		out = append(out, d)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type storeRequest struct {
	Name         *string  `json:"name" validate:"omitempty,max=80"`
	Address      *string  `json:"address" validate:"omitempty,max=300"`
	Lat          *float64 `json:"lat" validate:"omitempty,latitude"`
	Lng          *float64 `json:"lng" validate:"omitempty,longitude"`
	Phone        *string  `json:"phone" validate:"omitempty,max=30"`
	OpensAt      *string  `json:"opens_at"`
	ClosesAt     *string  `json:"closes_at"`
	RadiusKM     *float64 `json:"delivery_radius_km" validate:"omitempty,gt=0,lte=50"`
	IsActive     *bool    `json:"is_active"`
	IsTempClosed *bool    `json:"is_temporarily_closed"`
	SortOrder    *int     `json:"sort_order" validate:"omitempty,min=0,max=1000"`
}

func (req *storeRequest) normalize() (opens, closes *int, err error) {
	var details []httpx.FieldDetail
	for _, p := range []*string{req.Name, req.Address, req.Phone} {
		if p != nil {
			*p = strings.TrimSpace(*p)
		}
	}
	if req.Name != nil && *req.Name == "" {
		details = append(details, httpx.FieldDetail{Field: "name", Message: "wajib diisi"})
	}
	if req.Address != nil && *req.Address == "" {
		details = append(details, httpx.FieldDetail{Field: "address", Message: "wajib diisi"})
	}
	clock := func(field string, v *string) *int {
		if v == nil {
			return nil
		}
		m, ok := catalog.ParseClock(*v)
		if !ok {
			details = append(details, httpx.FieldDetail{Field: field, Message: "format jam harus HH:MM"})
			return nil
		}
		return &m
	}
	opens, closes = clock("opens_at", req.OpensAt), clock("closes_at", req.ClosesAt)
	if len(details) > 0 {
		return nil, nil, httpx.Validation("Data outlet belum lengkap.", details...)
	}
	return opens, closes, nil
}

func clockSQL(m *int) *string {
	if m == nil {
		return nil
	}
	v := catalog.FormatClock(*m)
	return &v
}

// POST /v1/admin/stores → 201. Wajib name, address, lat, lng. Id berurutan: ST04, ST05, …
func (h *Handler) CreateStore(w http.ResponseWriter, r *http.Request) {
	var req storeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	opens, closes, err := req.normalize()
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var missing []httpx.FieldDetail
	for field, ok := range map[string]bool{"name": req.Name != nil, "address": req.Address != nil, "lat": req.Lat != nil, "lng": req.Lng != nil} {
		if !ok {
			missing = append(missing, httpx.FieldDetail{Field: field, Message: "wajib diisi"})
		}
	}
	if len(missing) > 0 {
		httpx.FromError(w, r, httpx.Validation("Data outlet belum lengkap.", missing...))
		return
	}
	ctx := r.Context()
	var id string
	err = database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, storeIDLockKey); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO stores (id, name, address, lat, lng, phone, opens_at, closes_at, delivery_radius_km, is_open, sort_order)
			SELECT 'ST' || CASE WHEN n.next < 10 THEN '0' ELSE '' END || n.next::text,
			       $1, $2, $3, $4, NULLIF($5, ''), COALESCE($6::time, '08:00'), COALESCE($7::time, '22:00'),
			       COALESCE($8, 5), NOT COALESCE($9, FALSE),
			       COALESCE($10, (SELECT COALESCE(max(sort_order), 0) + 1 FROM stores))
			FROM (SELECT COALESCE(max(substring(id FROM '^ST([0-9]+)$')::int), 0) + 1 AS next FROM stores) n
			RETURNING id`,
			*req.Name, *req.Address, *req.Lat, *req.Lng, deref(req.Phone), clockSQL(opens), clockSQL(closes),
			req.RadiusKM, req.IsTempClosed, req.SortOrder).Scan(&id)
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.respondStore(w, r, id, http.StatusCreated, "Outlet ditambahkan", "STORE_CREATE", "Outlet baru %s")
}

// PATCH /v1/admin/stores/{id}
func (h *Handler) UpdateStore(w http.ResponseWriter, r *http.Request) {
	var req storeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	opens, closes, err := req.normalize()
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	tag, err := h.pool.Exec(ctx, `
		UPDATE stores SET
			name = COALESCE($2, name), address = COALESCE($3, address),
			lat = COALESCE($4, lat), lng = COALESCE($5, lng),
			phone = CASE WHEN $6::text IS NULL THEN phone ELSE NULLIF($6, '') END,
			opens_at = COALESCE($7::time, opens_at), closes_at = COALESCE($8::time, closes_at),
			delivery_radius_km = COALESCE($9, delivery_radius_km),
			is_active = COALESCE($10, is_active),
			is_open = CASE WHEN $11::bool IS NULL THEN is_open ELSE NOT $11 END,
			sort_order = COALESCE($12, sort_order)
		WHERE id = $1`, id, req.Name, req.Address, req.Lat, req.Lng, req.Phone, clockSQL(opens), clockSQL(closes),
		req.RadiusKM, req.IsActive, req.IsTempClosed, req.SortOrder)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.FromError(w, r, httpx.NotFound("Outlet tidak ditemukan."))
		return
	}
	summary := "Outlet %s diperbarui"
	switch {
	case req.IsTempClosed != nil && *req.IsTempClosed:
		summary = "Outlet %s ditutup sementara"
	case req.IsTempClosed != nil:
		summary = "Outlet %s dibuka kembali"
	case req.IsActive != nil && !*req.IsActive:
		summary = "Outlet %s dinonaktifkan"
	}
	h.respondStore(w, r, id, http.StatusOK, "Outlet diperbarui", "STORE_UPDATE", summary)
}

func (h *Handler) respondStore(w http.ResponseWriter, r *http.Request, id string, status int, message, action, summary string) {
	ctx := r.Context()
	s, ok, err := h.catalog.StoreByID(ctx, id, false)
	if err == nil && !ok {
		err = httpx.NotFound("Outlet tidak ditemukan.")
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	d, err := h.storeDTO(ctx, s)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, action, "store", id, fmt.Sprintf(summary, s.Name), d)
	httpx.JSONMessage(w, status, message, d)
}

type storeStockRequest struct {
	IsAvailable *bool `json:"is_available" validate:"required"`
}

// PUT /v1/admin/stores/{id}/products/{product_id} { is_available }
// Habis di outlet ini saja; tersedia = ikut status pusat (baris override dihapus).
func (h *Handler) SetStoreStock(w http.ResponseWriter, r *http.Request) {
	var req storeStockRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	storeID, productID := chi.URLParam(r, "id"), chi.URLParam(r, "product_id")
	s, ok, err := h.catalog.StoreByID(ctx, storeID, false)
	if err == nil && !ok {
		err = httpx.NotFound("Outlet tidak ditemukan.")
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, err := h.product(ctx, productID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if *req.IsAvailable {
		_, err = h.pool.Exec(ctx, `DELETE FROM store_product_availability WHERE store_id = $1 AND product_id = $2`, storeID, productID)
	} else {
		_, err = h.pool.Exec(ctx, `
			INSERT INTO store_product_availability (store_id, product_id, is_available) VALUES ($1, $2, FALSE)
			ON CONFLICT (store_id, product_id) DO UPDATE SET is_available = FALSE, updated_at = now()`, storeID, productID)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	state := map[bool]string{true: "tersedia", false: "habis"}[*req.IsAvailable]
	h.audit(r, "STORE_STOCK", "product", productID, fmt.Sprintf("%s %s di %s", p.ProductName, state, s.Name),
		map[string]any{"store_id": storeID, "is_available": *req.IsAvailable})
	p, err = h.productAt(ctx, productID, storeID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Stok outlet diperbarui", p)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
