package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// Status tampilan promo (dihitung, tidak disimpan).
const (
	DealActive    = "ACTIVE"
	DealScheduled = "SCHEDULED"
	DealExpired   = "EXPIRED"
	DealInactive  = "INACTIVE"
)

type DealDTO struct {
	ID           string  `json:"id"`
	ProductID    string  `json:"product_id"`
	ProductName  string  `json:"product_name"`
	ProductPrice int64   `json:"product_price"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle"`
	Badge        string  `json:"badge"`
	ImageURL     *string `json:"image_url"`
	DealPrice    int64   `json:"deal_price"`
	DiscountPct  int     `json:"discount_percent"`
	StartsAt     string  `json:"starts_at"`
	EndsAt       string  `json:"ends_at"`
	IsActive     bool    `json:"is_active"`
	ShowAsPopup  bool    `json:"show_as_popup"`
	SortOrder    int     `json:"sort_order"`
	Status       string  `json:"status"`
}

const dealSelect = `
	SELECT d.id, d.product_id, p.name, p.price, d.title, d.subtitle, d.badge, d.image_url, d.deal_price,
	       d.starts_at, d.ends_at, d.is_active, d.show_as_popup, d.sort_order
	FROM product_deals d JOIN products p ON p.id = d.product_id `

func (h *Handler) scanDeal(row pgx.CollectableRow) (DealDTO, error) {
	var d DealDTO
	var starts, ends time.Time
	err := row.Scan(&d.ID, &d.ProductID, &d.ProductName, &d.ProductPrice, &d.Title, &d.Subtitle, &d.Badge, &d.ImageURL,
		&d.DealPrice, &starts, &ends, &d.IsActive, &d.ShowAsPopup, &d.SortOrder)
	if err != nil {
		return d, err
	}
	d.StartsAt, d.EndsAt = httpx.ISO(starts), httpx.ISO(ends)
	if d.ProductPrice > 0 && d.DealPrice < d.ProductPrice {
		d.DiscountPct = int((d.ProductPrice - d.DealPrice) * 100 / d.ProductPrice)
	}
	now := h.now()
	switch {
	case !d.IsActive:
		d.Status = DealInactive
	case !now.Before(ends):
		d.Status = DealExpired
	case now.Before(starts):
		d.Status = DealScheduled
	default:
		d.Status = DealActive
	}
	return d, nil
}

// GET /v1/admin/deals — semua promo, yang berjalan & terjadwal dulu.
func (h *Handler) Deals(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), dealSelect+`
		ORDER BY (d.is_active AND d.ends_at > now()) DESC, d.sort_order, d.ends_at DESC, d.id`)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, h.scanDeal)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (h *Handler) deal(ctx context.Context, id string) (DealDTO, error) {
	rows, err := h.pool.Query(ctx, dealSelect+"WHERE d.id = $1", id)
	if err != nil {
		return DealDTO{}, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, h.scanDeal)
	if database.IsNoRows(err) {
		return d, httpx.NotFound("Promo tidak ditemukan.")
	}
	return d, err
}

// dealRequest dipakai POST (semua wajib kecuali yang punya default) dan PATCH (semua opsional).
type dealRequest struct {
	ProductID   *string    `json:"product_id" validate:"omitempty,min=1,max=50"`
	Title       *string    `json:"title" validate:"omitempty,max=120"`
	Subtitle    *string    `json:"subtitle" validate:"omitempty,max=200"`
	Badge       *string    `json:"badge" validate:"omitempty,max=30"`
	ImageURL    *string    `json:"image_url" validate:"omitempty,max=1000"`
	DealPrice   *int64     `json:"deal_price" validate:"omitempty,min=1,max=10000000"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	IsActive    *bool      `json:"is_active"`
	ShowAsPopup *bool      `json:"show_as_popup"`
	SortOrder   *int       `json:"sort_order" validate:"omitempty,min=0,max=1000"`
}

// dealValues: nilai lengkap setelah request digabung dengan data lama.
type dealValues struct {
	ProductID, Title, Subtitle, Badge string
	ImageURL                          *string
	DealPrice                         int64
	StartsAt, EndsAt                  time.Time
	IsActive, ShowAsPopup             bool
	SortOrder                         int
}

func (req dealRequest) merge(base dealValues) dealValues {
	v := base
	trim := func(s *string) string { return strings.TrimSpace(*s) }
	if req.ProductID != nil {
		v.ProductID = trim(req.ProductID)
	}
	if req.Title != nil {
		v.Title = trim(req.Title)
	}
	if req.Subtitle != nil {
		v.Subtitle = trim(req.Subtitle)
	}
	if req.Badge != nil {
		v.Badge = trim(req.Badge)
	}
	if req.ImageURL != nil {
		if u := trim(req.ImageURL); u == "" {
			v.ImageURL = nil
		} else {
			v.ImageURL = &u
		}
	}
	if req.DealPrice != nil {
		v.DealPrice = *req.DealPrice
	}
	if req.StartsAt != nil {
		v.StartsAt = *req.StartsAt
	}
	if req.EndsAt != nil {
		v.EndsAt = *req.EndsAt
	}
	if req.IsActive != nil {
		v.IsActive = *req.IsActive
	}
	if req.ShowAsPopup != nil {
		v.ShowAsPopup = *req.ShowAsPopup
	}
	if req.SortOrder != nil {
		v.SortOrder = *req.SortOrder
	}
	return v
}

// validate memeriksa aturan yang butuh DB: produk ada & harga promo < harga normal.
func (h *Handler) validateDeal(ctx context.Context, v dealValues) error {
	var details []httpx.FieldDetail
	if v.ProductID == "" {
		details = append(details, httpx.FieldDetail{Field: "product_id", Message: "wajib diisi"})
	}
	if v.Title == "" {
		details = append(details, httpx.FieldDetail{Field: "title", Message: "wajib diisi"})
	}
	if v.DealPrice <= 0 {
		details = append(details, httpx.FieldDetail{Field: "deal_price", Message: "wajib diisi"})
	}
	if v.EndsAt.IsZero() {
		details = append(details, httpx.FieldDetail{Field: "ends_at", Message: "wajib diisi"})
	} else if !v.EndsAt.After(v.StartsAt) {
		details = append(details, httpx.FieldDetail{Field: "ends_at", Message: "harus setelah tanggal mulai"})
	}
	if v.ImageURL != nil && !strings.HasPrefix(*v.ImageURL, "https://") {
		details = append(details, httpx.FieldDetail{Field: "image_url", Message: "harus diawali https://"})
	}
	if len(details) > 0 {
		return httpx.Validation("Data promo belum lengkap.", details...)
	}
	var price int64
	err := h.pool.QueryRow(ctx, `SELECT price FROM products WHERE id = $1`, v.ProductID).Scan(&price)
	if database.IsNoRows(err) {
		return httpx.Validation("Produk tidak ditemukan.", httpx.FieldDetail{Field: "product_id", Message: "produk tidak ada"})
	}
	if err != nil {
		return err
	}
	if v.DealPrice >= price {
		return &httpx.AppError{Status: http.StatusUnprocessableEntity, Type: "DEAL_PRICE_TOO_HIGH",
			Message: fmt.Sprintf("Harga promo harus lebih murah dari harga normal (%s).", rupiah(price)),
			Details: []httpx.FieldDetail{{Field: "deal_price", Message: "harus < " + rupiah(price)}}}
	}
	return nil
}

// POST /v1/admin/deals → 201
func (h *Handler) CreateDeal(w http.ResponseWriter, r *http.Request) {
	var req dealRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	v := req.merge(dealValues{StartsAt: h.now(), IsActive: true})
	ctx := r.Context()
	if err := h.validateDeal(ctx, v); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var id string
	err := h.pool.QueryRow(ctx, `
		INSERT INTO product_deals (id, product_id, title, subtitle, badge, image_url, deal_price, starts_at, ends_at,
		                           is_active, show_as_popup, sort_order)
		VALUES ('DL-' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 10), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`, v.ProductID, v.Title, v.Subtitle, v.Badge, v.ImageURL, v.DealPrice, v.StartsAt, v.EndsAt,
		v.IsActive, v.ShowAsPopup, v.SortOrder).Scan(&id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	d, err := h.deal(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "DEAL_CREATE", "deal", id,
		fmt.Sprintf("Promo baru %q: %s %s → %s", d.Title, d.ProductName, rupiah(d.ProductPrice), rupiah(d.DealPrice)), d)
	httpx.JSONMessage(w, http.StatusCreated, "Promo dibuat", d)
}

// PATCH /v1/admin/deals/{id}
func (h *Handler) UpdateDeal(w http.ResponseWriter, r *http.Request) {
	var req dealRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	cur, err := h.deal(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	starts, _ := time.Parse(time.RFC3339, cur.StartsAt)
	ends, _ := time.Parse(time.RFC3339, cur.EndsAt)
	v := req.merge(dealValues{
		ProductID: cur.ProductID, Title: cur.Title, Subtitle: cur.Subtitle, Badge: cur.Badge, ImageURL: cur.ImageURL,
		DealPrice: cur.DealPrice, StartsAt: starts, EndsAt: ends, IsActive: cur.IsActive, ShowAsPopup: cur.ShowAsPopup,
		SortOrder: cur.SortOrder,
	})
	if err := h.validateDeal(ctx, v); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE product_deals SET product_id = $2, title = $3, subtitle = $4, badge = $5, image_url = $6, deal_price = $7,
		       starts_at = $8, ends_at = $9, is_active = $10, show_as_popup = $11, sort_order = $12
		WHERE id = $1`, id, v.ProductID, v.Title, v.Subtitle, v.Badge, v.ImageURL, v.DealPrice, v.StartsAt, v.EndsAt,
		v.IsActive, v.ShowAsPopup, v.SortOrder); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	d, err := h.deal(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "DEAL_UPDATE", "deal", id, fmt.Sprintf("Promo %q diperbarui (%s)", d.Title, d.Status), d)
	httpx.JSONMessage(w, http.StatusOK, "Promo diperbarui", d)
}

// DELETE /v1/admin/deals/{id}
func (h *Handler) DeleteDeal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	d, err := h.deal(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM product_deals WHERE id = $1`, id); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "DEAL_DELETE", "deal", id, fmt.Sprintf("Promo %q dihapus", d.Title), d)
	httpx.JSONMessage(w, http.StatusOK, "Promo dihapus", nil)
}
