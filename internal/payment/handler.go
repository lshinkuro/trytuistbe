package payment

import (
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
)

type StoreDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type OrderRefDTO struct {
	ID          string `json:"id"`
	OrderNumber string `json:"order_number"`
	Status      string `json:"status"`
}

type PaymentDTO struct {
	ID              string         `json:"id"`
	Provider        string         `json:"provider"`
	ProviderOrderID string         `json:"provider_order_id"`
	Status          string         `json:"status"`
	Amount          int64          `json:"amount"`
	ExpiresAt       string         `json:"expires_at"`
	RedirectURL     *string        `json:"redirect_url"`
	FinishURLPrefix string         `json:"finish_url_prefix"`
	PaymentType     *string        `json:"payment_type"`
	PaidAt          *string        `json:"paid_at"`
	StatusMessage   *string        `json:"status_message"`
	RefundStatus    *string        `json:"refund_status"`
	Store           StoreDTO       `json:"store"`
	Quote           order.QuoteDTO `json:"quote"`
	Order           *OrderRefDTO   `json:"order"`
	CreatedAt       string         `json:"created_at"`
}

func (s *Service) toDTO(p Payment) PaymentDTO {
	dto := PaymentDTO{
		ID: p.ID, Provider: "MIDTRANS", ProviderOrderID: p.ProviderOrderID, Status: p.Status, Amount: p.Amount,
		ExpiresAt: httpx.ISO(p.ExpiresAt), FinishURLPrefix: s.FinishURL(), PaymentType: p.PaymentType,
		PaidAt: httpx.ISOPtr(p.PaidAt), RefundStatus: p.RefundStatus,
		Store: StoreDTO{ID: p.StoreID, Name: p.StoreName}, Quote: order.QuoteDTOOf(p.Snapshot.Quote),
		CreatedAt: httpx.ISO(p.CreatedAt),
	}
	if p.Status == StatusPending {
		dto.RedirectURL = p.RedirectURL
	}
	if p.StatusMessage != nil && *p.StatusMessage != "" {
		dto.StatusMessage = p.StatusMessage
	}
	if p.OrderID != nil && p.OrderNumber != nil {
		st := ""
		if p.OrderStatus != nil {
			st = *p.OrderStatus
		}
		dto.Order = &OrderRefDTO{ID: *p.OrderID, OrderNumber: *p.OrderNumber, Status: st}
	}
	return dto
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// POST /v1/checkout 🔒 — COD → pesanan; online → sesi Midtrans.
func (h *Handler) Checkout(w http.ResponseWriter, r *http.Request) {
	in, err := order.DecodeCreate(r)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	res, err := h.svc.Checkout(r.Context(), httpx.UserID(r.Context()), in)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if res.Order != nil {
		httpx.JSONMessage(w, http.StatusCreated, "Pesanan diterima", map[string]any{
			"next": "ORDER", "order": order.ToOrderDTO(*res.Order),
		})
		return
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	httpx.JSONMessage(w, status, "Silakan selesaikan pembayaran", map[string]any{
		"next": "PAYMENT", "payment": h.svc.toDTO(*res.Payment),
	})
}

// GET /v1/payments/{id} 🔒
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Get(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.svc.toDTO(p))
}

// GET /v1/payments?status=PENDING 🔒
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	var statuses []string
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.ToUpper(strings.TrimSpace(s))
			switch s {
			case "":
			case StatusPending, StatusPaid, StatusFailed, StatusExpired, StatusCancelled:
				statuses = append(statuses, s)
			default:
				httpx.FromError(w, r, httpx.Validation("Parameter tidak valid.", httpx.FieldDetail{
					Field: "status", Message: "harus salah satu dari: PENDING, PAID, FAILED, EXPIRED, CANCELLED"}))
				return
			}
		}
	}
	list, err := h.svc.List(r.Context(), httpx.UserID(r.Context()), statuses)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := make([]PaymentDTO, len(list))
	for i, p := range list {
		out[i] = h.svc.toDTO(p)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// POST /v1/payments/{id}/cancel 🔒
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Cancel(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Pembayaran dibatalkan", h.svc.toDTO(p))
}

// POST /v1/payments/midtrans/notification — publik (tanpa X-API-Key), divalidasi signature_key.
func (h *Handler) Notification(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		httpx.FromError(w, r, httpx.Validation("Body tidak bisa dibaca."))
		return
	}
	if !h.svc.Enabled() {
		httpx.FromError(w, r, httpx.NotFound("Pembayaran online tidak aktif."))
		return
	}
	if err := h.svc.HandleNotification(r.Context(), body); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// GET /v1/payments/finish — finish URL Snap. Di iOS URL ini dicegat WebView;
// halaman ini hanya untuk browser biasa.
func (h *Handler) Finish(w http.ResponseWriter, r *http.Request) {
	orderID := html.EscapeString(r.URL.Query().Get("order_id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html><html lang="id"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>TryTuist</title>
<body style="font-family:-apple-system,system-ui,sans-serif;text-align:center;padding:48px 24px;color:#222">
<h2>Terima kasih!</h2><p>Kembali ke aplikasi TryTuist untuk melihat status pembayaran.</p>
<p style="color:#888;font-size:13px">`+orderID+`</p></body></html>`)
}
