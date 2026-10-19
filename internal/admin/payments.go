package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
	"trytuist-api/internal/payment"
)

// Web admin → Pembayaran (fase 5b, §5.27): sesi Midtrans, status, dan refund.

var paymentStatuses = []string{payment.StatusPending, payment.StatusPaid, payment.StatusFailed, payment.StatusExpired, payment.StatusCancelled}

type AdminPaymentSummaryDTO struct {
	ID              string               `json:"id"`
	ProviderOrderID string               `json:"provider_order_id"`
	Status          string               `json:"status"`
	Amount          int64                `json:"amount"`
	RefundedAmount  int64                `json:"refunded_amount"`
	RefundStatus    *string              `json:"refund_status"`
	PaymentType     *string              `json:"payment_type"`
	StoreID         string               `json:"store_id"`
	StoreName       string               `json:"store_name"`
	Customer        CustomerDTO          `json:"customer"`
	Order           *payment.OrderRefDTO `json:"order"`
	CreatedAt       string               `json:"created_at"`
	PaidAt          *string              `json:"paid_at"`
	ExpiresAt       string               `json:"expires_at"`
}

// GET /v1/admin/payments?status=PAID,PENDING&q=&from=&to=&store_id=&refund=1&page=&limit=
func (h *Handler) Payments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := httpx.ParsePage(q, orderListLimit)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	statuses := []string{}
	for _, s := range strings.Split(q.Get("status"), ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s == "" {
			continue
		}
		if !contains(paymentStatuses, s) {
			httpx.FromError(w, r, httpx.Validation("Filter status tidak valid.",
				httpx.FieldDetail{Field: "status", Message: "harus salah satu dari: " + strings.Join(paymentStatuses, ", ")}))
			return
		}
		statuses = append(statuses, s)
	}
	rng, hasRange, err := ParseDateRange(q, h.loc, h.now(), 0)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var from, to *time.Time
	if hasRange {
		from, to = &rng.From, &rng.To
	}
	ctx := r.Context()
	storeID, err := scopeStore(ctx, q.Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	search := strings.TrimSpace(q.Get("q"))
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search) + "%"
	refundOnly := q.Get("refund") == "1"

	where := `WHERE (cardinality($1::text[]) = 0 OR p.status::text = ANY($1))
		AND ($2::timestamptz IS NULL OR p.created_at >= $2)
		AND ($3::timestamptz IS NULL OR p.created_at < $3)
		AND ($4 = '' OR p.provider_order_id ILIKE $5 OR o.order_number ILIKE $5 OR u.name ILIKE $5 OR u.email ILIKE $5)
		AND ($6 = '' OR p.store_id = $6)
		AND (NOT $7 OR p.refund_status IS NOT NULL)`
	from_ := ` FROM payments p JOIN users u ON u.id = p.user_id JOIN stores s ON s.id = p.store_id
		LEFT JOIN orders o ON o.id = p.order_id `
	args := []any{statuses, from, to, search, like, storeID, refundOnly}

	var total int
	if err := h.pool.QueryRow(ctx, `SELECT count(*)`+from_+where, args...).Scan(&total); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT p.id::text, p.provider_order_id, p.status::text, p.amount,
		       COALESCE((SELECT sum(amount) FROM payment_refunds WHERE payment_id = p.id AND status = 'SUCCEEDED'), 0),
		       p.refund_status, p.payment_type, s.id, s.name,
		       u.id::text, u.name, u.email, u.phone,
		       o.id::text, o.order_number, o.status::text,
		       p.created_at, p.paid_at, p.expires_at`+from_+where+`
		ORDER BY p.created_at DESC, p.id LIMIT $8 OFFSET $9`, append(args, page.Limit, page.Offset())...)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminPaymentSummaryDTO, error) {
		var d AdminPaymentSummaryDTO
		var orderID, orderNumber, orderStatus *string
		var created, expires time.Time
		var paid *time.Time
		err := row.Scan(&d.ID, &d.ProviderOrderID, &d.Status, &d.Amount, &d.RefundedAmount, &d.RefundStatus, &d.PaymentType,
			&d.StoreID, &d.StoreName, &d.Customer.ID, &d.Customer.Name, &d.Customer.Email, &d.Customer.Phone,
			&orderID, &orderNumber, &orderStatus, &created, &paid, &expires)
		if orderID != nil {
			d.Order = &payment.OrderRefDTO{ID: *orderID, OrderNumber: *orderNumber, Status: *orderStatus}
		}
		d.CreatedAt, d.PaidAt, d.ExpiresAt = httpx.ISO(created), httpx.ISOPtr(paid), httpx.ISO(expires)
		return d, err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list, page.Meta(total))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

type RefundDTO struct {
	ID        string  `json:"id"`
	Amount    int64   `json:"amount"`
	Reason    string  `json:"reason"`
	Method    string  `json:"method"` // MIDTRANS | MANUAL
	Status    string  `json:"status"` // PENDING | SUCCEEDED | FAILED
	Message   *string `json:"message"`
	AdminName *string `json:"admin_name"`
	CreatedAt string  `json:"created_at"`
}

type PaymentEventDTO struct {
	Source         string          `json:"source"`
	ProviderStatus *string         `json:"provider_status"`
	SignatureOK    *bool           `json:"signature_ok"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      string          `json:"created_at"`
}

type AdminPaymentDTO struct {
	payment.PaymentDTO
	ProviderTxnID    *string           `json:"provider_txn_id"`
	Customer         CustomerDTO       `json:"customer"`
	RefundedAmount   int64             `json:"refunded_amount"`
	RefundableAmount int64             `json:"refundable_amount"`
	CanRefundOnline  bool              `json:"can_refund_online"` // false → hanya refund manual
	Refunds          []RefundDTO       `json:"refunds"`
	Events           []PaymentEventDTO `json:"events"`
}

// scopedPayment: pembayaran outlet lain tidak terlihat oleh admin outlet (404).
func (h *Handler) scopedPayment(ctx context.Context, id string) (payment.Payment, error) {
	p, err := h.payments.AdminGet(ctx, id)
	if err != nil {
		return p, err
	}
	if a := adminFrom(ctx); !a.IsSuper() && p.StoreID != a.StoreID {
		return payment.Payment{}, httpx.NotFound("Pembayaran tidak ditemukan.")
	}
	return p, nil
}

func (h *Handler) paymentDTO(ctx context.Context, p payment.Payment) (AdminPaymentDTO, error) {
	dto := AdminPaymentDTO{PaymentDTO: h.payments.DTO(p), ProviderTxnID: p.ProviderTxnID,
		Refunds: []RefundDTO{}, Events: []PaymentEventDTO{}}
	if err := h.pool.QueryRow(ctx, `SELECT id::text, name, email, phone FROM users WHERE id = $1`, p.UserID).
		Scan(&dto.Customer.ID, &dto.Customer.Name, &dto.Customer.Email, &dto.Customer.Phone); err != nil {
		return dto, err
	}
	refunds, err := h.payments.Refunds(ctx, p.ID)
	if err != nil {
		return dto, err
	}
	var reserved int64
	for _, rf := range refunds {
		dto.Refunds = append(dto.Refunds, RefundDTO{ID: rf.ID, Amount: rf.Amount, Reason: rf.Reason, Method: rf.Method,
			Status: rf.Status, Message: rf.Message, AdminName: rf.AdminName, CreatedAt: httpx.ISO(rf.CreatedAt)})
		switch rf.Status {
		case "SUCCEEDED":
			dto.RefundedAmount += rf.Amount
			reserved += rf.Amount
		case "PENDING":
			reserved += rf.Amount
		}
	}
	if p.Status == payment.StatusPaid {
		dto.RefundableAmount = max(p.Amount-reserved, 0)
		dto.CanRefundOnline = h.payments.Enabled() && payment.RefundableOnline(p.PaymentType)
	}
	events, err := h.payments.Events(ctx, p.ID)
	if err != nil {
		return dto, err
	}
	for _, e := range events {
		dto.Events = append(dto.Events, PaymentEventDTO{Source: e.Source, ProviderStatus: e.ProviderStatus,
			SignatureOK: e.SignatureOK, Payload: e.Payload, CreatedAt: httpx.ISO(e.CreatedAt)})
	}
	return dto, nil
}

// GET /v1/admin/payments/{id}
func (h *Handler) Payment(w http.ResponseWriter, r *http.Request) {
	p, err := h.scopedPayment(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	dto, err := h.paymentDTO(r.Context(), p)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

type refundRequest struct {
	Amount int64  `json:"amount" validate:"gte=0"` // 0 / kosong = seluruh sisa
	Reason string `json:"reason" validate:"required,max=300"`
	Manual bool   `json:"manual"`
}

// POST /v1/admin/payments/{id}/refund { amount, reason, manual } — khusus super admin.
func (h *Handler) RefundPayment(w http.ResponseWriter, r *http.Request) {
	var req refundRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	p, err := h.scopedPayment(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rf, err := h.payments.Refund(ctx, adminFrom(ctx).ID, p.ID, payment.RefundInput{Amount: req.Amount, Reason: req.Reason, Manual: req.Manual})
	if rf.ID != "" {
		// Dicatat juga saat Midtrans menolak, supaya percobaan refund terlihat di audit log.
		h.audit(r, "PAYMENT_REFUND", "payment", p.ID,
			fmt.Sprintf("Refund %s %s (%s): %s", p.ProviderOrderID, order.Rupiah(rf.Amount), strings.ToLower(rf.Method), rf.Status),
			map[string]any{"refund_id": rf.ID, "amount": rf.Amount, "method": rf.Method, "status": rf.Status, "reason": rf.Reason})
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, err = h.payments.AdminGet(ctx, p.ID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	dto, err := h.paymentDTO(ctx, p)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Refund berhasil dicatat", dto)
}

// orderPaymentRef: ringkasan pembayaran online di detail pesanan admin (nil untuk COD).
type orderPaymentRefDTO struct {
	ID              string  `json:"id"`
	ProviderOrderID string  `json:"provider_order_id"`
	Status          string  `json:"status"`
	PaymentType     *string `json:"payment_type"`
	RefundStatus    *string `json:"refund_status"`
	PaidAt          *string `json:"paid_at"`
}

func (h *Handler) orderPayment(ctx context.Context, orderID string) (*orderPaymentRefDTO, error) {
	var d orderPaymentRefDTO
	var paid *time.Time
	err := h.pool.QueryRow(ctx, `
		SELECT id::text, provider_order_id, status::text, payment_type, refund_status, paid_at
		FROM payments WHERE order_id = $1`, orderID).
		Scan(&d.ID, &d.ProviderOrderID, &d.Status, &d.PaymentType, &d.RefundStatus, &paid)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	d.PaidAt = httpx.ISOPtr(paid)
	return &d, err
}
