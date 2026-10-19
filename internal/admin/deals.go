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
	StoreID      *string `json:"store_id"` // null = semua outlet (§5.29)
	StoreName    *string `json:"store_name"`
	CanEdit      bool    `json:"can_edit"` // admin outlet tidak bisa mengubah promo pusat
}

const dealSelect = `
	SELECT d.id, d.product_id, p.name, COALESCE(sp.price, p.price), d.title, d.subtitle, d.badge, d.image_url, d.deal_price,
	       d.starts_at, d.ends_at, d.is_active, d.show_as_popup, d.sort_order, d.store_id, s.name
	FROM product_deals d JOIN products p ON p.id = d.product_id
	LEFT JOIN stores s ON s.id = d.store_id
	LEFT JOIN store_products sp ON sp.store_id = d.store_id AND sp.product_id = d.product_id `

func (h *Handler) scanDeal(row pgx.CollectableRow) (DealDTO, error) {
	var d DealDTO
	var starts, ends time.Time
	err := row.Scan(&d.ID, &d.ProductID, &d.ProductName, &d.ProductPrice, &d.Title, &d.Subtitle, &d.Badge, &d.ImageURL,
		&d.DealPrice, &starts, &ends, &d.IsActive, &d.ShowAsPopup, &d.SortOrder, &d.StoreID, &d.StoreName)
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

// GET /v1/admin/deals?store_id= — promo, yang berjalan & terjadwal dulu.
// Super admin tanpa store_id: semua promo. Dengan outlet (atau admin outlet): promo pusat + promo outlet itu.
func (h *Handler) Deals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	storeID, err := scopeStore(ctx, r.URL.Query().Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, dealSelect+`
		WHERE $1 = '' OR d.store_id IS NULL OR d.store_id = $1
		ORDER BY (d.is_active AND d.ends_at > now()) DESC, d.sort_order, d.ends_at DESC, d.id`, storeID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, h.scanDeal)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	for i := range list {
		list[i].CanEdit = canEditDeal(ctx, list[i]) == nil
	}
	httpx.JSON(w, http.StatusOK, list)
}

// canEditDeal: admin outlet hanya promo outletnya; promo pusat (store_id null) khusus super admin.
func canEditDeal(ctx context.Context, d DealDTO) error {
	a := adminFrom(ctx)
	if a.IsSuper() || (d.StoreID != nil && *d.StoreID == a.StoreID) {
		return nil
	}
	return httpx.NewError(http.StatusForbidden, "FORBIDDEN", "Promo ini milik pusat atau outlet lain.")
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
	d.CanEdit = canEditDeal(ctx, d) == nil
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
	// StoreID: "" = semua outlet. Hanya super admin; admin outlet selalu outletnya sendiri.
	StoreID *string `json:"store_id" validate:"omitempty,max=50"`
}

// dealValues: nilai lengkap setelah request digabung dengan data lama.
type dealValues struct {
	ProductID, Title, Subtitle, Badge string
	ImageURL                          *string
	DealPrice                         int64
	StartsAt, EndsAt                  time.Time
	IsActive, ShowAsPopup             bool
	SortOrder                         int
	StoreID                           string // "" = semua outlet
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
	if req.StoreID != nil {
		v.StoreID = trim(req.StoreID)
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
	if v.StoreID != "" {
		if _, ok, err := h.catalog.StoreByID(ctx, v.StoreID, false); err != nil || !ok {
			if err != nil {
				return err
			}
			return httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
		}
	}
	// Promo outlet dibandingkan dengan harga di outlet itu (bisa harga khusus outlet) dan menunya harus dijual di sana.
	var price int64
	var listed bool
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(sp.price, p.price), $2 = '' OR sp.product_id IS NOT NULL
		FROM products p LEFT JOIN store_products sp ON sp.product_id = p.id AND sp.store_id = $2
		WHERE p.id = $1`, v.ProductID, v.StoreID).Scan(&price, &listed)
	if database.IsNoRows(err) {
		return httpx.Validation("Produk tidak ditemukan.", httpx.FieldDetail{Field: "product_id", Message: "produk tidak ada"})
	}
	if err != nil {
		return err
	}
	if !listed {
		return httpx.Validation("Menu ini tidak dijual di outlet tersebut.", httpx.FieldDetail{Field: "product_id", Message: "tidak dijual di outlet"})
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
	if a := adminFrom(ctx); !a.IsSuper() {
		if v.StoreID != "" && v.StoreID != a.StoreID {
			httpx.FromError(w, r, errOtherStore)
			return
		}
		v.StoreID = a.StoreID
	}
	if err := h.validateDeal(ctx, v); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var id string
	err := h.pool.QueryRow(ctx, `
		INSERT INTO product_deals (id, product_id, title, subtitle, badge, image_url, deal_price, starts_at, ends_at,
		                           is_active, show_as_popup, sort_order, store_id)
		VALUES ('DL-' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 10), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		        NULLIF($12, ''))
		RETURNING id`, v.ProductID, v.Title, v.Subtitle, v.Badge, v.ImageURL, v.DealPrice, v.StartsAt, v.EndsAt,
		v.IsActive, v.ShowAsPopup, v.SortOrder, v.StoreID).Scan(&id)
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
	if err == nil {
		err = canEditDeal(ctx, cur)
	}
	if err == nil && req.StoreID != nil && !adminFrom(ctx).IsSuper() {
		err = errNotSuper // admin outlet tidak bisa memindahkan promo ke outlet lain
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	starts, _ := time.Parse(time.RFC3339, cur.StartsAt)
	ends, _ := time.Parse(time.RFC3339, cur.EndsAt)
	v := req.merge(dealValues{
		ProductID: cur.ProductID, Title: cur.Title, Subtitle: cur.Subtitle, Badge: cur.Badge, ImageURL: cur.ImageURL,
		DealPrice: cur.DealPrice, StartsAt: starts, EndsAt: ends, IsActive: cur.IsActive, ShowAsPopup: cur.ShowAsPopup,
		SortOrder: cur.SortOrder, StoreID: deref(cur.StoreID),
	})
	if err := h.validateDeal(ctx, v); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE product_deals SET product_id = $2, title = $3, subtitle = $4, badge = $5, image_url = $6, deal_price = $7,
		       starts_at = $8, ends_at = $9, is_active = $10, show_as_popup = $11, sort_order = $12, store_id = NULLIF($13, '')
		WHERE id = $1`, id, v.ProductID, v.Title, v.Subtitle, v.Badge, v.ImageURL, v.DealPrice, v.StartsAt, v.EndsAt,
		v.IsActive, v.ShowAsPopup, v.SortOrder, v.StoreID); err != nil {
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
	if err == nil {
		err = canEditDeal(ctx, d)
	}
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
