package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/httpx"
)

type AdminCustomerDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Phone       *string `json:"phone"`
	Role        string  `json:"role"`
	MemberTier  string  `json:"member_tier"`
	OrdersCount int     `json:"orders_count"`
	TotalSpent  int64   `json:"total_spent"` // dari pesanan COMPLETED
	LastOrderAt *string `json:"last_order_at"`
	CreatedAt   string  `json:"created_at"`
}

// GET /v1/admin/customers?q=&page=&limit= — pelanggan + ringkasan pembeliannya.
func (h *Handler) Customers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := httpx.ParsePage(q, 20)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	search := strings.TrimSpace(q.Get("q"))
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search) + "%"
	where := `WHERE ($1 = '' OR u.name ILIKE $2 OR u.email ILIKE $2 OR u.phone ILIKE $2)`
	ctx := r.Context()
	var total int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM users u `+where, search, like).Scan(&total); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT u.id::text, u.name, u.email, u.phone, u.role, u.member_tier,
		       COALESCE(s.orders, 0), COALESCE(s.spent, 0), s.last_at, u.created_at
		FROM users u
		LEFT JOIN LATERAL (
			SELECT count(*)::int AS orders,
			       COALESCE(sum(total) FILTER (WHERE status = 'COMPLETED'), 0)::bigint AS spent,
			       max(created_at) AS last_at
			FROM orders WHERE user_id = u.id
		) s ON true
		`+where+`
		ORDER BY s.last_at DESC NULLS LAST, u.created_at DESC
		LIMIT $3 OFFSET $4`, search, like, page.Limit, page.Offset())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminCustomerDTO, error) {
		var d AdminCustomerDTO
		var last *time.Time
		var created time.Time
		err := row.Scan(&d.ID, &d.Name, &d.Email, &d.Phone, &d.Role, &d.MemberTier, &d.OrdersCount, &d.TotalSpent, &last, &created)
		d.LastOrderAt, d.CreatedAt = httpx.ISOPtr(last), httpx.ISO(created)
		return d, err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list, page.Meta(total))
}
