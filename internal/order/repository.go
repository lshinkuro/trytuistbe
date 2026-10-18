package order

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
)

// Status pesanan (enum order_status di DB).
const (
	StatusPending    = "PENDING"
	StatusPaid       = "PAID"
	StatusProcessing = "PROCESSING"
	StatusDelivering = "DELIVERING"
	StatusCompleted  = "COMPLETED"
	StatusCancelled  = "CANCELLED"
	StatusFailed     = "FAILED"
)

var AllStatuses = []string{StatusPending, StatusPaid, StatusProcessing, StatusDelivering, StatusCompleted, StatusCancelled, StatusFailed}

type Store struct {
	ID      string
	Name    string
	Address string
	IsOpen  bool
}

type Address struct {
	ID        string
	Label     string
	Recipient string
	Phone     string
	Address   string
	Notes     *string
	Lat, Lng  *float64
}

type StatusEvent struct {
	Status string
	At     time.Time
}

type Rating struct {
	Rating    int
	Comment   *string
	CreatedAt time.Time
}

type Order struct {
	ID            string
	OrderNumber   string
	UserID        string
	Status        string
	Subtotal      int64
	DeliveryFee   int64
	Discount      int64
	Total         int64
	PaymentMethod string
	// Snapshot akun e-wallet saat checkout (nil untuk COD/QRIS).
	PaymentAccountID     *string
	PaymentProvider      *string
	PaymentAccountMasked *string
	Note                 *string
	VoucherID            *string
	VoucherCode          *string
	CreatedAt            time.Time
	Store                Store
	Address              *Address
	Items                []Line
	History              []StatusEvent
	Rating               *Rating
}

type Summary struct {
	ID          string
	OrderNumber string
	StoreName   string
	FirstItem   string
	ItemCount   int
	Qty         int
	Total       int64
	Status      string
	CreatedAt   time.Time
}

type Repository struct{}

func (Repository) StoreByID(ctx context.Context, db database.DBTX, id string) (Store, error) {
	var s Store
	err := db.QueryRow(ctx, `SELECT id, name, address, is_open FROM stores WHERE id = $1`, id).
		Scan(&s.ID, &s.Name, &s.Address, &s.IsOpen)
	return s, err
}

// DefaultStoreID: outlet buka pertama (dipakai bila iOS belum mengirim store_id).
func (Repository) DefaultStoreID(ctx context.Context, db database.DBTX) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id FROM stores ORDER BY is_open DESC, id LIMIT 1`).Scan(&id)
	return id, err
}

func (Repository) AddressBelongsTo(ctx context.Context, db database.DBTX, id, userID string) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM addresses WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&ok)
	return ok, err
}

func (Repository) VoucherByCode(ctx context.Context, db database.DBTX, code string) (Voucher, error) {
	var v Voucher
	err := db.QueryRow(ctx, `
		SELECT id::text, code, title, description, discount_type, discount_value, min_subtotal,
		       max_discount, starts_at, ends_at, quota
		FROM vouchers WHERE upper(code) = upper($1)`, code).
		Scan(&v.ID, &v.Code, &v.Title, &v.Description, &v.DiscountType, &v.DiscountValue, &v.MinSubtotal,
			&v.MaxDiscount, &v.StartsAt, &v.EndsAt, &v.Quota)
	return v, err
}

func (Repository) VoucherUsedByUser(ctx context.Context, db database.DBTX, userID, voucherID string) (bool, error) {
	var used bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM user_vouchers WHERE user_id = $1 AND voucher_id = $2 AND used_at IS NOT NULL)`,
		userID, voucherID).Scan(&used)
	return used, err
}

func (Repository) VoucherUsageCount(ctx context.Context, db database.DBTX, voucherID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM orders WHERE voucher_id = $1 AND status NOT IN ('CANCELLED','FAILED')`, voucherID).Scan(&n)
	return n, err
}

// MarkVoucherUsed menandai voucher terpakai; false bila sudah terpakai (race).
func (Repository) MarkVoucherUsed(ctx context.Context, db database.DBTX, userID, voucherID string) (bool, error) {
	tag, err := db.Exec(ctx, `
		INSERT INTO user_vouchers (user_id, voucher_id, used_at) VALUES ($1, $2, now())
		ON CONFLICT (user_id, voucher_id) DO UPDATE SET used_at = now() WHERE user_vouchers.used_at IS NULL`,
		userID, voucherID)
	return tag.RowsAffected() == 1, err
}

func (Repository) ReleaseVoucher(ctx context.Context, db database.DBTX, userID, voucherID string) error {
	_, err := db.Exec(ctx, `UPDATE user_vouchers SET used_at = NULL WHERE user_id = $1 AND voucher_id = $2`, userID, voucherID)
	return err
}

func (Repository) OrderIDByIdempotencyKey(ctx context.Context, db database.DBTX, userID, key string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id::text FROM orders WHERE user_id = $1 AND idempotency_key = $2`, userID, key).Scan(&id)
	return id, err
}

// NextOrderNumber: ORD-YYYYMMDD-NNNN, urut per hari. Dikunci advisory lock
// supaya dua checkout bersamaan tidak mendapat nomor yang sama.
func (Repository) NextOrderNumber(ctx context.Context, tx pgx.Tx, day time.Time) (string, error) {
	prefix := "ORD-" + day.Format("20060102") + "-"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, prefix); err != nil {
		return "", err
	}
	var last int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(substring(order_number FROM $2::int)::int), 0)
		FROM orders WHERE order_number LIKE $1 || '%'`, prefix, len(prefix)+1).Scan(&last)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%04d", prefix, last+1), nil
}

type insertOrder struct {
	OrderNumber    string
	UserID         string
	Quote          Quote
	PaymentMethod  string
	Payment        PaymentChoice
	Note           *string
	IdempotencyKey *string
}

func (Repository) InsertOrder(ctx context.Context, tx pgx.Tx, in insertOrder) (string, error) {
	q := in.Quote
	var voucherID *string
	if q.Voucher != nil {
		voucherID = &q.Voucher.ID
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (order_number, user_id, store_id, address_id, subtotal, delivery_fee, discount, total,
		                    voucher_id, payment_method, note, idempotency_key,
		                    payment_account_id, payment_provider, payment_account_masked)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id::text`,
		in.OrderNumber, in.UserID, q.StoreID, q.AddressID, q.Subtotal, q.DeliveryFee, q.Discount, q.Total,
		voucherID, in.Payment.Method, in.Note, in.IdempotencyKey,
		in.Payment.AccountID, in.Payment.Provider, in.Payment.AccountMasked).Scan(&id)
	if err != nil {
		return "", err
	}
	batch := &pgx.Batch{}
	for i, l := range q.Lines {
		batch.Queue(`
			INSERT INTO order_items (order_id, product_id, product_name, option_id, option_name, unit_price, quantity, note, subtotal, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, l.ProductID, l.ProductName, l.OptionID, l.OptionName, l.UnitPrice, l.Quantity, l.Note, l.Subtotal, i)
		batch.Queue(`UPDATE products SET sold_count = sold_count + $2 WHERE id = $1`, l.ProductID, l.Quantity)
	}
	batch.Queue(`INSERT INTO order_status_history (order_id, status) VALUES ($1, 'PENDING')`, id)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return "", err
	}
	return id, nil
}

// OrderStatusForUpdate mengunci order milik user; pgx.ErrNoRows bila tidak ada.
func (Repository) OrderStatusForUpdate(ctx context.Context, tx pgx.Tx, id, userID string) (status string, voucherID *string, err error) {
	err = tx.QueryRow(ctx, `SELECT status::text, voucher_id::text FROM orders WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID).
		Scan(&status, &voucherID)
	return status, voucherID, err
}

func (Repository) SetStatus(ctx context.Context, tx pgx.Tx, id, status string) error {
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2::order_status, updated_at = now() WHERE id = $1`, id, status); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status) VALUES ($1, $2::order_status)`, id, status)
	return err
}

// RevertSoldCount dipanggil saat pesanan batal/gagal.
func (Repository) RevertSoldCount(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE products p SET sold_count = GREATEST(p.sold_count - oi.qty, 0)
		FROM (SELECT product_id, sum(quantity)::int AS qty FROM order_items WHERE order_id = $1 GROUP BY product_id) oi
		WHERE p.id = oi.product_id`, orderID)
	return err
}

func (Repository) InsertRating(ctx context.Context, tx pgx.Tx, orderID string, rating int, comment *string) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_ratings (order_id, rating, comment) VALUES ($1, $2, $3)`, orderID, rating, comment)
	return err
}

// RecalcProductRatings: rating produk = rata-rata rating semua pesanan yang memuat produk itu.
func (Repository) RecalcProductRatings(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE products p SET rating = sub.avg_rating, updated_at = now()
		FROM (
			SELECT oi.product_id, round(avg(r.rating)::numeric, 1) AS avg_rating
			FROM order_ratings r
			JOIN (SELECT DISTINCT order_id, product_id FROM order_items) oi ON oi.order_id = r.order_id
			WHERE oi.product_id IN (SELECT product_id FROM order_items WHERE order_id = $1)
			GROUP BY oi.product_id
		) sub
		WHERE p.id = sub.product_id`, orderID)
	return err
}

func (Repository) List(ctx context.Context, db database.DBTX, userID string, statuses []string, limit, offset int) ([]Summary, int, error) {
	var total int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM orders
		WHERE user_id = $1 AND (cardinality($2::text[]) = 0 OR status::text = ANY($2))`,
		userID, statuses).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(ctx, `
		SELECT o.id::text, o.order_number, s.name, o.total, o.status::text, o.created_at,
		       COALESCE(agg.first_item, ''), COALESCE(agg.item_count, 0), COALESCE(agg.qty, 0)
		FROM orders o
		JOIN stores s ON s.id = o.store_id
		LEFT JOIN LATERAL (
			SELECT (array_agg(product_name ORDER BY sort_order))[1] AS first_item,
			       count(*)::int AS item_count, sum(quantity)::int AS qty
			FROM order_items WHERE order_id = o.id
		) agg ON true
		WHERE o.user_id = $1 AND (cardinality($2::text[]) = 0 OR o.status::text = ANY($2))
		ORDER BY o.created_at DESC, o.order_number DESC
		LIMIT $3 OFFSET $4`, userID, statuses, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Summary, error) {
		var s Summary
		err := row.Scan(&s.ID, &s.OrderNumber, &s.StoreName, &s.Total, &s.Status, &s.CreatedAt,
			&s.FirstItem, &s.ItemCount, &s.Qty)
		return s, err
	})
	return list, total, err
}

// Get mengembalikan detail lengkap pesanan milik user; pgx.ErrNoRows bila tidak ada.
func (Repository) Get(ctx context.Context, db database.DBTX, id, userID string) (Order, error) {
	var o Order
	var addrID *string
	err := db.QueryRow(ctx, `
		SELECT o.id::text, o.order_number, o.user_id::text, o.status::text, o.subtotal, o.delivery_fee, o.discount, o.total,
		       o.payment_method, o.note, o.voucher_id::text, v.code, o.created_at,
		       s.id, s.name, s.address, s.is_open, o.address_id::text,
		       o.payment_account_id::text, o.payment_provider, o.payment_account_masked
		FROM orders o
		JOIN stores s ON s.id = o.store_id
		LEFT JOIN vouchers v ON v.id = o.voucher_id
		WHERE o.id = $1 AND o.user_id = $2`, id, userID).
		Scan(&o.ID, &o.OrderNumber, &o.UserID, &o.Status, &o.Subtotal, &o.DeliveryFee, &o.Discount, &o.Total,
			&o.PaymentMethod, &o.Note, &o.VoucherID, &o.VoucherCode, &o.CreatedAt,
			&o.Store.ID, &o.Store.Name, &o.Store.Address, &o.Store.IsOpen, &addrID,
			&o.PaymentAccountID, &o.PaymentProvider, &o.PaymentAccountMasked)
	if err != nil {
		return o, err
	}

	if addrID != nil {
		var a Address
		err := db.QueryRow(ctx, `
			SELECT id::text, label, recipient, phone, address, notes, lat, lng FROM addresses WHERE id = $1`, *addrID).
			Scan(&a.ID, &a.Label, &a.Recipient, &a.Phone, &a.Address, &a.Notes, &a.Lat, &a.Lng)
		if err == nil {
			o.Address = &a
		} else if !database.IsNoRows(err) {
			return o, err
		}
	}

	rows, err := db.Query(ctx, `
		SELECT product_id, product_name, option_id, option_name, unit_price, quantity, note, subtotal
		FROM order_items WHERE order_id = $1 ORDER BY sort_order`, o.ID)
	if err != nil {
		return o, err
	}
	o.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Line, error) {
		var l Line
		err := row.Scan(&l.ProductID, &l.ProductName, &l.OptionID, &l.OptionName, &l.UnitPrice, &l.Quantity, &l.Note, &l.Subtotal)
		return l, err
	})
	if err != nil {
		return o, err
	}

	rows, err = db.Query(ctx, `SELECT status::text, at FROM order_status_history WHERE order_id = $1 ORDER BY at, status`, o.ID)
	if err != nil {
		return o, err
	}
	o.History, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatusEvent, error) {
		var e StatusEvent
		err := row.Scan(&e.Status, &e.At)
		return e, err
	})
	if err != nil {
		return o, err
	}

	var r Rating
	err = db.QueryRow(ctx, `SELECT rating, comment, created_at FROM order_ratings WHERE order_id = $1`, o.ID).
		Scan(&r.Rating, &r.Comment, &r.CreatedAt)
	if err == nil {
		o.Rating = &r
	} else if !database.IsNoRows(err) {
		return o, err
	}
	return o, nil
}

// DefaultPaymentMethod: 'COD' | 'QRIS' | uuid e-wallet (users.default_payment_method).
func (Repository) DefaultPaymentMethod(ctx context.Context, db database.DBTX, userID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT default_payment_method FROM users WHERE id = $1`, userID).Scan(&id)
	return id, err
}

// PaymentAccount: e-wallet tersimpan milik user; pgx.ErrNoRows bila bukan miliknya / sudah dihapus.
func (Repository) PaymentAccount(ctx context.Context, db database.DBTX, id, userID string) (provider, number string, err error) {
	err = db.QueryRow(ctx, `SELECT provider, account_number FROM user_payment_methods WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&provider, &number)
	return provider, number, err
}

// DefaultAddressID: alamat utama user; pgx.ErrNoRows bila user belum punya alamat.
func (Repository) DefaultAddressID(ctx context.Context, db database.DBTX, userID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id::text FROM addresses WHERE user_id = $1 AND is_default`, userID).Scan(&id)
	return id, err
}
