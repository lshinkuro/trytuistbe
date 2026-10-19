package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
)

type CustomerDTO struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone"`
}

type AdminOrderSummaryDTO struct {
	OrderID       string      `json:"order_id"`
	OrderNumber   string      `json:"order_number"`
	Status        string      `json:"status"`
	Total         int64       `json:"total"`
	PaymentMethod string      `json:"payment_method"`
	ItemSummary   string      `json:"item_summary"`
	Qty           int         `json:"qty"`
	StoreID       string      `json:"store_id"`
	StoreName     string      `json:"store_name"`
	Customer      CustomerDTO `json:"customer"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
}

type AdminStatusEventDTO struct {
	Status    string  `json:"status"`
	At        string  `json:"at"`
	Note      *string `json:"note"`
	ChangedBy *string `json:"changed_by"`
}

type AdminOrderDTO struct {
	order.OrderDTO
	StatusHistory []AdminStatusEventDTO `json:"status_history"`
	Customer      CustomerDTO           `json:"customer"`
	NextStatuses  []string              `json:"next_statuses"`
	Payment       *orderPaymentRefDTO   `json:"payment"` // null untuk COD
}

const orderListLimit = 20

// GET /v1/admin/orders?status=PENDING,PAID&q=&from=&to=&page=&limit=
func (h *Handler) Orders(w http.ResponseWriter, r *http.Request) {
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
		if !isStatus(s) {
			httpx.FromError(w, r, httpx.Validation("Filter status tidak valid.",
				httpx.FieldDetail{Field: "status", Message: "harus salah satu dari: " + strings.Join(order.AllStatuses, ", ")}))
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
	search := strings.TrimSpace(q.Get("q"))
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search) + "%"

	where := `WHERE (cardinality($1::text[]) = 0 OR o.status::text = ANY($1))
		AND ($2::timestamptz IS NULL OR o.created_at >= $2)
		AND ($3::timestamptz IS NULL OR o.created_at < $3)
		AND ($4 = '' OR o.order_number ILIKE $5 OR u.name ILIKE $5 OR u.email ILIKE $5)
		AND ($8 = '' OR o.store_id = $8)`
	ctx := r.Context()
	storeID, err := scopeStore(ctx, q.Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var total int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM orders o JOIN users u ON u.id = o.user_id `+
		strings.ReplaceAll(where, "$8", "$6"), statuses, from, to, search, like, storeID).Scan(&total); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT o.id::text, o.order_number, o.status::text, o.total, o.payment_method, s.id, s.name,
		       COALESCE(agg.first_item, ''), COALESCE(agg.item_count, 0), COALESCE(agg.qty, 0),
		       u.id::text, u.name, u.email, u.phone, o.created_at, o.updated_at
		FROM orders o
		JOIN users u ON u.id = o.user_id
		JOIN stores s ON s.id = o.store_id
		LEFT JOIN LATERAL (
			SELECT (array_agg(product_name ORDER BY sort_order))[1] AS first_item,
			       count(*)::int AS item_count, sum(quantity)::int AS qty
			FROM order_items WHERE order_id = o.id
		) agg ON true
		`+where+`
		ORDER BY o.created_at DESC, o.order_number DESC
		LIMIT $6 OFFSET $7`, statuses, from, to, search, like, page.Limit, page.Offset(), storeID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminOrderSummaryDTO, error) {
		var d AdminOrderSummaryDTO
		var first string
		var count int
		var created, updated time.Time
		err := row.Scan(&d.OrderID, &d.OrderNumber, &d.Status, &d.Total, &d.PaymentMethod, &d.StoreID, &d.StoreName,
			&first, &count, &d.Qty, &d.Customer.ID, &d.Customer.Name, &d.Customer.Email, &d.Customer.Phone,
			&created, &updated)
		d.ItemSummary = first
		if count > 1 {
			d.ItemSummary = fmt.Sprintf("%s +%d lainnya", first, count-1)
		}
		d.CreatedAt, d.UpdatedAt = httpx.ISO(created), httpx.ISO(updated)
		return d, err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list, page.Meta(total))
}

func isStatus(s string) bool {
	for _, x := range order.AllStatuses {
		if x == s {
			return true
		}
	}
	return false
}

func (h *Handler) orderDTO(r *http.Request, o order.Order) (AdminOrderDTO, error) {
	dto := AdminOrderDTO{OrderDTO: order.ToOrderDTO(o), NextStatuses: order.AdminNextStatuses(o.Status)}
	dto.StatusHistory = make([]AdminStatusEventDTO, len(o.History))
	for i, e := range o.History {
		dto.StatusHistory[i] = AdminStatusEventDTO{Status: e.Status, At: httpx.ISO(e.At), Note: e.Note, ChangedBy: e.ChangedBy}
	}
	err := h.pool.QueryRow(r.Context(), `SELECT id::text, name, email, phone FROM users WHERE id = $1`, o.UserID).
		Scan(&dto.Customer.ID, &dto.Customer.Name, &dto.Customer.Email, &dto.Customer.Phone)
	if err != nil {
		return dto, err
	}
	dto.Payment, err = h.orderPayment(r.Context(), o.ID)
	return dto, err
}

// scopedOrder: pesanan outlet lain tidak terlihat oleh admin outlet (404, bukan 403).
func (h *Handler) scopedOrder(ctx context.Context, id string) (order.Order, error) {
	o, err := h.orders.AdminGet(ctx, id)
	if err != nil {
		return o, err
	}
	if a := adminFrom(ctx); !a.IsSuper() && o.Store.ID != a.StoreID {
		return order.Order{}, httpx.NotFound("Pesanan tidak ditemukan.")
	}
	return o, nil
}

// GET /v1/admin/orders/{id}
func (h *Handler) Order(w http.ResponseWriter, r *http.Request) {
	o, err := h.scopedOrder(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	dto, err := h.orderDTO(r, o)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

type updateStatusRequest struct {
	Status string `json:"status" validate:"required"`
	Note   string `json:"note" validate:"max=300"`
}

// PATCH /v1/admin/orders/{id}/status { status, note }
// Sukses = COMPLETED, Gagal = FAILED (alasan wajib), Batal = CANCELLED (alasan wajib).
func (h *Handler) UpdateOrderStatus(w http.ResponseWriter, r *http.Request) {
	var req updateStatusRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	to := strings.ToUpper(strings.TrimSpace(req.Status))
	if !isStatus(to) {
		httpx.FromError(w, r, httpx.Validation("Status tidak valid.",
			httpx.FieldDetail{Field: "status", Message: "harus salah satu dari: " + strings.Join(order.AllStatuses, ", ")}))
		return
	}
	a := adminFrom(r.Context())
	if _, err := h.scopedOrder(r.Context(), chi.URLParam(r, "id")); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, from, err := h.orders.AdminSetStatus(r.Context(), chi.URLParam(r, "id"), to, order.StatusMeta{Note: req.Note, ActorID: a.ID})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "ORDER_STATUS", "order", o.ID,
		fmt.Sprintf("Status %s: %s → %s", o.OrderNumber, from, to),
		map[string]any{"from": from, "to": to, "note": strings.TrimSpace(req.Note)})
	dto, err := h.orderDTO(r, o)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Status pesanan diperbarui", dto)
}
