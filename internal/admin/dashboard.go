package admin

import (
	"context"
	"math"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/httpx"
)

// Definisi angka dashboard (§5.25):
//   - Penjualan = pesanan berstatus COMPLETED (sukses) yang dibuat dalam rentang.
//   - Pembelian = semua transaksi pembelian pelanggan yang dibuat dalam rentang,
//     apa pun statusnya (termasuk yang masih diproses, gagal, dan batal).

type SalesDTO struct {
	Revenue           int64    `json:"revenue"`
	OrdersCompleted   int      `json:"orders_completed"`
	AverageOrderValue int64    `json:"average_order_value"`
	ItemsSold         int      `json:"items_sold"`
	DiscountTotal     int64    `json:"discount_total"`
	DeliveryFeeTotal  int64    `json:"delivery_fee_total"`
	RevenueChangePct  *float64 `json:"revenue_change_pct"` // nil bila periode sebelumnya 0
}

type PurchasesDTO struct {
	Orders          int      `json:"orders"`
	Buyers          int      `json:"buyers"`
	NewBuyers       int      `json:"new_buyers"`
	RepeatBuyers    int      `json:"repeat_buyers"`
	InProgress      int      `json:"in_progress"`
	Failed          int      `json:"failed"`
	Cancelled       int      `json:"cancelled"`
	SuccessRatePct  float64  `json:"success_rate_pct"`
	GrossValue      int64    `json:"gross_value"` // total semua transaksi
	OrdersChangePct *float64 `json:"orders_change_pct"`
}

type DailyDTO struct {
	Date            string `json:"date"`
	Revenue         int64  `json:"revenue"`
	Orders          int    `json:"orders"`
	OrdersCompleted int    `json:"orders_completed"`
}

type StatusCountDTO struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
	Total  int64  `json:"total"`
}

type TopProductDTO struct {
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int    `json:"quantity"`
	Revenue     int64  `json:"revenue"`
}

type PaymentMethodStatDTO struct {
	Method string `json:"method"`
	Count  int    `json:"count"`
	Total  int64  `json:"total"`
}

type DashboardDTO struct {
	Range          map[string]string      `json:"range"`
	Sales          SalesDTO               `json:"sales"`
	Purchases      PurchasesDTO           `json:"purchases"`
	Daily          []DailyDTO             `json:"daily"`
	ByStatus       []StatusCountDTO       `json:"by_status"`
	TopProducts    []TopProductDTO        `json:"top_products"`
	PaymentMethods []PaymentMethodStatDTO `json:"payment_methods"`
	// StoreID: outlet yang difilter (nil = semua outlet).
	StoreID *string `json:"store_id"`
	// ByStore: perbandingan antar-outlet (hanya bila tidak difilter per outlet).
	ByStore []StoreStatDTO `json:"by_store"`
}

type StoreStatDTO struct {
	StoreID        string  `json:"store_id"`
	StoreName      string  `json:"store_name"`
	Orders         int     `json:"orders"`
	Completed      int     `json:"orders_completed"`
	Revenue        int64   `json:"revenue"`
	SuccessRatePct float64 `json:"success_rate_pct"`
}

// GET /v1/admin/dashboard?from=&to=&store_id= (default 30 hari terakhir, semua outlet)
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	rng, _, err := ParseDateRange(r.URL.Query(), h.loc, h.now(), 30)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	storeID := ""
	if s := strings.TrimSpace(r.URL.Query().Get("store_id")); s != "" {
		store, ok, err := h.catalog.StoreByID(r.Context(), s, false)
		if err == nil && !ok {
			err = httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
		}
		if err != nil {
			httpx.FromError(w, r, err)
			return
		}
		storeID = store.ID
	}
	d, err := h.dashboard(r.Context(), rng, storeID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

type periodTotals struct {
	revenue, gross, discount, delivery int64
	completed, orders, buyers          int
	inProgress, failed, cancelled      int
}

func (h *Handler) totals(ctx context.Context, rng DateRange, storeID string) (periodTotals, error) {
	var t periodTotals
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(total) FILTER (WHERE status = 'COMPLETED'), 0)::bigint,
		       COALESCE(sum(total), 0)::bigint,
		       COALESCE(sum(discount) FILTER (WHERE status = 'COMPLETED'), 0)::bigint,
		       COALESCE(sum(delivery_fee) FILTER (WHERE status = 'COMPLETED'), 0)::bigint,
		       count(*) FILTER (WHERE status = 'COMPLETED'),
		       count(*),
		       count(DISTINCT user_id),
		       count(*) FILTER (WHERE status IN ('PENDING','PAID','PROCESSING','DELIVERING')),
		       count(*) FILTER (WHERE status = 'FAILED'),
		       count(*) FILTER (WHERE status = 'CANCELLED')
		FROM orders WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR store_id = $3)`, rng.From, rng.To, storeID).
		Scan(&t.revenue, &t.gross, &t.discount, &t.delivery, &t.completed, &t.orders, &t.buyers,
			&t.inProgress, &t.failed, &t.cancelled)
	return t, err
}

func changePct(now, before int64) *float64 {
	if before == 0 {
		return nil
	}
	v := math.Round(float64(now-before)/float64(before)*1000) / 10
	return &v
}

func (h *Handler) dashboard(ctx context.Context, rng DateRange, storeID string) (DashboardDTO, error) {
	cur, err := h.totals(ctx, rng, storeID)
	if err != nil {
		return DashboardDTO{}, err
	}
	prev, err := h.totals(ctx, rng.Previous(), storeID)
	if err != nil {
		return DashboardDTO{}, err
	}
	d := DashboardDTO{Range: map[string]string{
		"from": rng.DayFrom.Format("2006-01-02"), "to": rng.DayTo.Format("2006-01-02"),
	}, ByStore: []StoreStatDTO{}}
	if storeID != "" {
		d.StoreID = &storeID
	}
	d.Sales = SalesDTO{
		Revenue: cur.revenue, OrdersCompleted: cur.completed, DiscountTotal: cur.discount, DeliveryFeeTotal: cur.delivery,
		RevenueChangePct: changePct(cur.revenue, prev.revenue),
	}
	if cur.completed > 0 {
		d.Sales.AverageOrderValue = cur.revenue / int64(cur.completed)
	}
	d.Purchases = PurchasesDTO{
		Orders: cur.orders, Buyers: cur.buyers, InProgress: cur.inProgress, Failed: cur.failed, Cancelled: cur.cancelled,
		GrossValue: cur.gross, OrdersChangePct: changePct(int64(cur.orders), int64(prev.orders)),
	}
	if cur.orders > 0 {
		d.Purchases.SuccessRatePct = math.Round(float64(cur.completed)/float64(cur.orders)*1000) / 10
	}

	// Pembeli baru = pesanan pertamanya (sepanjang waktu) ada di rentang ini.
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT user_id, min(created_at) AS first_at FROM orders
			WHERE ($3 = '' OR store_id = $3)
			  AND user_id IN (SELECT DISTINCT user_id FROM orders
			                  WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR store_id = $3))
			GROUP BY user_id
		) f WHERE f.first_at >= $1`, rng.From, rng.To, storeID).Scan(&d.Purchases.NewBuyers); err != nil {
		return d, err
	}
	d.Purchases.RepeatBuyers = d.Purchases.Buyers - d.Purchases.NewBuyers

	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(oi.quantity), 0)::int FROM order_items oi JOIN orders o ON o.id = oi.order_id
		WHERE o.status = 'COMPLETED' AND o.created_at >= $1 AND o.created_at < $2
		  AND ($3 = '' OR o.store_id = $3)`, rng.From, rng.To, storeID).
		Scan(&d.Sales.ItemsSold); err != nil {
		return d, err
	}

	// Harian (hari kosong tetap muncul dengan angka 0). Tanggal dihitung di zona waktu toko.
	_, offset := rng.From.Zone()
	rows, err := h.pool.Query(ctx, `
		WITH days AS (
			SELECT generate_series($1::date, $2::date, interval '1 day')::date AS day
		), agg AS (
			SELECT (created_at AT TIME ZONE 'UTC' + make_interval(secs => $5::int))::date AS day,
			       COALESCE(sum(total) FILTER (WHERE status = 'COMPLETED'), 0)::bigint AS revenue,
			       count(*)::int AS orders,
			       count(*) FILTER (WHERE status = 'COMPLETED')::int AS completed
			FROM orders WHERE created_at >= $3 AND created_at < $4 AND ($6 = '' OR store_id = $6)
			GROUP BY 1
		)
		SELECT to_char(days.day, 'YYYY-MM-DD'), COALESCE(agg.revenue, 0), COALESCE(agg.orders, 0), COALESCE(agg.completed, 0)
		FROM days LEFT JOIN agg ON agg.day = days.day ORDER BY days.day`,
		rng.DayFrom.Format("2006-01-02"), rng.DayTo.Format("2006-01-02"), rng.From, rng.To, offset, storeID)
	if err != nil {
		return d, err
	}
	if d.Daily, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DailyDTO, error) {
		var x DailyDTO
		return x, row.Scan(&x.Date, &x.Revenue, &x.Orders, &x.OrdersCompleted)
	}); err != nil {
		return d, err
	}

	rows, err = h.pool.Query(ctx, `
		SELECT status::text, count(*)::int, COALESCE(sum(total), 0)::bigint FROM orders
		WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR store_id = $3)
		GROUP BY status ORDER BY status`, rng.From, rng.To, storeID)
	if err != nil {
		return d, err
	}
	if d.ByStatus, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatusCountDTO, error) {
		var x StatusCountDTO
		return x, row.Scan(&x.Status, &x.Count, &x.Total)
	}); err != nil {
		return d, err
	}

	rows, err = h.pool.Query(ctx, `
		SELECT oi.product_id, (array_agg(oi.product_name ORDER BY o.created_at DESC))[1],
		       sum(oi.quantity)::int, sum(oi.subtotal)::bigint
		FROM order_items oi JOIN orders o ON o.id = oi.order_id
		WHERE o.status = 'COMPLETED' AND o.created_at >= $1 AND o.created_at < $2 AND ($3 = '' OR o.store_id = $3)
		GROUP BY oi.product_id ORDER BY 4 DESC, 3 DESC LIMIT 5`, rng.From, rng.To, storeID)
	if err != nil {
		return d, err
	}
	if d.TopProducts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TopProductDTO, error) {
		var x TopProductDTO
		return x, row.Scan(&x.ProductID, &x.ProductName, &x.Quantity, &x.Revenue)
	}); err != nil {
		return d, err
	}

	rows, err = h.pool.Query(ctx, `
		SELECT payment_method, count(*)::int, COALESCE(sum(total), 0)::bigint FROM orders
		WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR store_id = $3)
		GROUP BY payment_method ORDER BY 2 DESC`, rng.From, rng.To, storeID)
	if err != nil {
		return d, err
	}
	if d.PaymentMethods, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PaymentMethodStatDTO, error) {
		var x PaymentMethodStatDTO
		return x, row.Scan(&x.Method, &x.Count, &x.Total)
	}); err != nil || storeID != "" {
		return d, err
	}

	// Perbandingan outlet: semua outlet aktif tampil, termasuk yang belum ada pesanan.
	rows, err = h.pool.Query(ctx, `
		SELECT s.id, s.name, COALESCE(a.orders, 0), COALESCE(a.completed, 0), COALESCE(a.revenue, 0)
		FROM stores s
		LEFT JOIN (
			SELECT store_id, count(*)::int AS orders,
			       count(*) FILTER (WHERE status = 'COMPLETED')::int AS completed,
			       COALESCE(sum(total) FILTER (WHERE status = 'COMPLETED'), 0)::bigint AS revenue
			FROM orders WHERE created_at >= $1 AND created_at < $2 GROUP BY store_id
		) a ON a.store_id = s.id
		WHERE s.is_active OR a.orders > 0
		ORDER BY 5 DESC, s.sort_order, s.id`, rng.From, rng.To)
	if err != nil {
		return d, err
	}
	d.ByStore, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoreStatDTO, error) {
		var x StoreStatDTO
		err := row.Scan(&x.StoreID, &x.StoreName, &x.Orders, &x.Completed, &x.Revenue)
		if x.Orders > 0 {
			x.SuccessRatePct = math.Round(float64(x.Completed)/float64(x.Orders)*1000) / 10
		}
		return x, err
	})
	return d, err
}
