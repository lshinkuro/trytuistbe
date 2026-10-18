package order

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"trytuist-api/internal/httpx"
)

// ===== Request DTO =====

type itemRequest struct {
	ProductID string  `json:"product_id" validate:"required,max=50"`
	OptionID  *string `json:"option_id" validate:"omitempty,max=50"`
	Quantity  int     `json:"quantity"` // divalidasi service → 422 INVALID_QUANTITY
	Note      string  `json:"note"`     // divalidasi service (≤ 100 karakter)
}

type quoteRequest struct {
	StoreID     string        `json:"store_id" validate:"max=50"`
	AddressID   string        `json:"address_id" validate:"max=64"`
	VoucherCode string        `json:"voucher_code" validate:"max=50"`
	Items       []itemRequest `json:"items" validate:"max=50,dive"`
}

type createRequest struct {
	quoteRequest
	// Kosong = pakai metode utama user. payment_method_id = "COD" | "QRIS" | uuid e-wallet tersimpan.
	PaymentMethod   string `json:"payment_method" validate:"omitempty,oneof=COD QRIS EWALLET ONLINE"`
	PaymentMethodID string `json:"payment_method_id" validate:"max=64"`
	Note            string `json:"note" validate:"max=255"`
}

type rateRequest struct {
	Rating  int    `json:"rating" validate:"required,min=1,max=5"`
	Comment string `json:"comment" validate:"max=500"`
}

type statusRequest struct {
	Status string `json:"status" validate:"required,oneof=PAID PROCESSING DELIVERING COMPLETED CANCELLED FAILED"`
}

func (r quoteRequest) toInput() QuoteInput {
	items := make([]ItemInput, len(r.Items))
	for i, it := range r.Items {
		opt := ""
		if it.OptionID != nil {
			opt = strings.TrimSpace(*it.OptionID)
		}
		items[i] = ItemInput{ProductID: it.ProductID, OptionID: opt, Quantity: it.Quantity, Note: it.Note}
	}
	return QuoteInput{StoreID: r.StoreID, AddressID: r.AddressID, VoucherCode: r.VoucherCode, Items: items}
}

// ===== Response DTO =====

type LineDTO struct {
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	OptionID    *string `json:"option_id"`
	OptionName  *string `json:"option_name"`
	UnitPrice   int64   `json:"unit_price"`
	Quantity    int     `json:"quantity"`
	Note        string  `json:"note"`
	Subtotal    int64   `json:"subtotal"`
}

type QuoteDTO struct {
	StoreID     string    `json:"store_id"`
	StoreName   string    `json:"store_name"`
	Items       []LineDTO `json:"items"`
	Subtotal    int64     `json:"subtotal"`
	DeliveryFee int64     `json:"delivery_fee"`
	Discount    int64     `json:"discount"`
	Total       int64     `json:"total"`
	VoucherCode *string   `json:"voucher_code"`
}

// OrderSummaryDTO = OrderDTO di iOS (tab Pesanan).
type OrderSummaryDTO struct {
	OrderID     string `json:"order_id"`
	OrderNumber string `json:"order_number"`
	StoreName   string `json:"store_name"`
	ProductName string `json:"product_name"`
	CreatedAt   string `json:"created_at"`
	TotalPrice  int64  `json:"total_price"`
	Qty         int    `json:"qty"`
	Status      string `json:"status"`
	CanRate     bool   `json:"can_rate"`
}

type StoreDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
}

type AddressDTO struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Recipient string   `json:"recipient"`
	Phone     string   `json:"phone"`
	Address   string   `json:"address"`
	Notes     *string  `json:"notes"`
	Lat       *float64 `json:"lat"`
	Lng       *float64 `json:"lng"`
}

type StatusEventDTO struct {
	Status string  `json:"status"`
	At     string  `json:"at"`
	Note   *string `json:"note,omitempty"` // alasan dari admin (mis. pesanan gagal)
}

type RatingDTO struct {
	Rating    int     `json:"rating"`
	Comment   *string `json:"comment"`
	CreatedAt string  `json:"created_at"`
}

// OrderDTO: detail pesanan (POST /v1/orders, GET /v1/orders/{id}).
type OrderDTO struct {
	OrderID       string `json:"order_id"`
	OrderNumber   string `json:"order_number"`
	Status        string `json:"status"`
	Subtotal      int64  `json:"subtotal"`
	DeliveryFee   int64  `json:"delivery_fee"`
	Discount      int64  `json:"discount"`
	Total         int64  `json:"total"`
	PaymentMethod string `json:"payment_method"`
	// "COD" | "QRIS" | uuid e-wallet (null bila e-wallet tanpa akun / sudah dihapus).
	PaymentMethodID      *string          `json:"payment_method_id"`
	PaymentProvider      *string          `json:"payment_provider"`
	PaymentAccountMasked *string          `json:"payment_account_masked"`
	Note                 *string          `json:"note"`
	VoucherCode          *string          `json:"voucher_code"`
	CreatedAt            string           `json:"created_at"`
	Items                []LineDTO        `json:"items"`
	Store                StoreDTO         `json:"store"`
	Address              *AddressDTO      `json:"address"`
	StatusHistory        []StatusEventDTO `json:"status_history"`
	Rating               *RatingDTO       `json:"rating"`
	CanCancel            bool             `json:"can_cancel"`
	CanRate              bool             `json:"can_rate"`
}

func toLineDTOs(lines []Line) []LineDTO {
	out := make([]LineDTO, len(lines))
	for i, l := range lines {
		out[i] = LineDTO{
			ProductID: l.ProductID, ProductName: l.ProductName, OptionID: l.OptionID, OptionName: l.OptionName,
			UnitPrice: l.UnitPrice, Quantity: l.Quantity, Note: l.Note, Subtotal: l.Subtotal,
		}
	}
	return out
}

func toQuoteDTO(q Quote) QuoteDTO {
	dto := QuoteDTO{
		StoreID: q.StoreID, StoreName: q.StoreName, Items: toLineDTOs(q.Lines),
		Subtotal: q.Subtotal, DeliveryFee: q.DeliveryFee, Discount: q.Discount, Total: q.Total,
	}
	if q.Voucher != nil {
		dto.VoucherCode = &q.Voucher.Code
	}
	return dto
}

func toSummaryDTO(s Summary) OrderSummaryDTO {
	name := s.FirstItem
	if s.ItemCount > 1 {
		name = fmt.Sprintf("%s +%d lainnya", s.FirstItem, s.ItemCount-1)
	}
	return OrderSummaryDTO{
		OrderID: s.ID, OrderNumber: s.OrderNumber, StoreName: s.StoreName, ProductName: name,
		CreatedAt: httpx.ISO(s.CreatedAt), TotalPrice: s.Total, Qty: s.Qty, Status: s.Status, CanRate: s.CanRate,
	}
}

func ToOrderDTO(o Order) OrderDTO {
	dto := OrderDTO{
		OrderID: o.ID, OrderNumber: o.OrderNumber, Status: o.Status,
		Subtotal: o.Subtotal, DeliveryFee: o.DeliveryFee, Discount: o.Discount, Total: o.Total,
		PaymentMethod: o.PaymentMethod, Note: o.Note, VoucherCode: o.VoucherCode,
		PaymentProvider: o.PaymentProvider, PaymentAccountMasked: o.PaymentAccountMasked,
		CreatedAt: httpx.ISO(o.CreatedAt),
		Items:     toLineDTOs(o.Items),
		Store:     StoreDTO{ID: o.Store.ID, Name: o.Store.Name, Address: o.Store.Address},
		CanCancel: o.Status == StatusPending,
		CanRate:   o.Status == StatusCompleted && o.Rating == nil,
	}
	switch {
	case o.PaymentAccountID != nil:
		dto.PaymentMethodID = o.PaymentAccountID
	case o.PaymentMethod != "EWALLET":
		id := o.PaymentMethod
		dto.PaymentMethodID = &id
	}
	if a := o.Address; a != nil {
		dto.Address = &AddressDTO{ID: a.ID, Label: a.Label, Recipient: a.Recipient, Phone: a.Phone,
			Address: a.Address, Notes: a.Notes, Lat: a.Lat, Lng: a.Lng}
	}
	dto.StatusHistory = make([]StatusEventDTO, len(o.History))
	for i, e := range o.History {
		dto.StatusHistory[i] = StatusEventDTO{Status: e.Status, At: httpx.ISO(e.At), Note: e.Note}
	}
	if r := o.Rating; r != nil {
		dto.Rating = &RatingDTO{Rating: r.Rating, Comment: r.Comment, CreatedAt: httpx.ISO(r.CreatedAt)}
	}
	return dto
}

// ===== Handler =====

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// POST /v1/orders/quote 🔒
func (h *Handler) Quote(w http.ResponseWriter, r *http.Request) {
	var req quoteRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	q, err := h.svc.Quote(r.Context(), httpx.UserID(r.Context()), req.toInput())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toQuoteDTO(q))
}

// POST /v1/orders 🔒 — header opsional Idempotency-Key.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, err := h.svc.Create(r.Context(), httpx.UserID(r.Context()), CreateInput{
		QuoteInput:      req.toInput(),
		PaymentMethod:   req.PaymentMethod,
		PaymentMethodID: req.PaymentMethodID,
		Note:            req.Note,
		IdempotencyKey:  r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusCreated, "Pesanan diterima", ToOrderDTO(o))
}

// GET /v1/orders 🔒 — ?status=PENDING,PROCESSING&from=&to=&sort=asc|desc&page=&limit=
// from/to: RFC3339 (iOS mengirim awal hari di zona waktu perangkat), rentang [from, to).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := httpx.ParsePage(q, 20)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var statuses []string
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.ToUpper(strings.TrimSpace(s))
			if s == "" {
				continue
			}
			if !isStatus(s) {
				httpx.FromError(w, r, httpx.Validation("Parameter tidak valid.", httpx.FieldDetail{
					Field: "status", Message: "harus salah satu dari: " + strings.Join(AllStatuses, ", "),
				}))
				return
			}
			statuses = append(statuses, s)
		}
	}
	filter := ListFilter{Statuses: statuses, Oldest: q.Get("sort") == "asc"}
	var details []httpx.FieldDetail
	for _, p := range []struct {
		field string
		dst   **time.Time
	}{{"from", &filter.From}, {"to", &filter.To}} {
		field, dst := p.field, p.dst
		if v := q.Get(field); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				details = append(details, httpx.FieldDetail{Field: field, Message: "format waktu harus RFC3339"})
				continue
			}
			*dst = &t
		}
	}
	if s := q.Get("sort"); s != "" && s != "asc" && s != "desc" {
		details = append(details, httpx.FieldDetail{Field: "sort", Message: "harus asc atau desc"})
	}
	if len(details) > 0 {
		httpx.FromError(w, r, httpx.Validation("Parameter tidak valid.", details...))
		return
	}
	list, total, err := h.svc.List(r.Context(), httpx.UserID(r.Context()), filter, page)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := make([]OrderSummaryDTO, len(list))
	for i, s := range list {
		out[i] = toSummaryDTO(s)
	}
	httpx.JSON(w, http.StatusOK, out, page.Meta(total))
}

// GET /v1/orders/{id} 🔒
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Get(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToOrderDTO(o))
}

// POST /v1/orders/{id}/cancel 🔒
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Cancel(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Pesanan dibatalkan", ToOrderDTO(o))
}

// POST /v1/orders/{id}/rating 🔒
func (h *Handler) Rate(w http.ResponseWriter, r *http.Request) {
	var req rateRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, err := h.svc.Rate(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"), req.Rating, req.Comment)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Terima kasih atas penilaianmu", ToOrderDTO(o))
}

// POST /v1/dev/orders/{id}/status 🔒 — HANYA aktif di APP_ENV=dev.
// Simulasi outlet/kurir supaya alur Diproses → Selesai → Beri Nilai bisa dites dari iOS.
func (h *Handler) DevSetStatus(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, err := h.svc.AdvanceStatus(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"), req.Status)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToOrderDTO(o))
}

func isStatus(s string) bool {
	for _, v := range AllStatuses {
		if v == s {
			return true
		}
	}
	return false
}
