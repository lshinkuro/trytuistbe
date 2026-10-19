package order

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
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
	Note   *string // alasan dari admin, mis. "Pembayaran ditolak"
	// ChangedBy: nama admin yang mengubah; nil = sistem / user sendiri.
	ChangedBy *string
}

// StatusMeta: keterangan perubahan status (dari web admin).
type StatusMeta struct {
	Note    string
	ActorID string // id admin; "" = sistem / user sendiri
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
	DistanceKM           *float64
	ETA                  *ETA
	FreeDeliveryCoupon   bool
	DeliveryFeeWaived    int64
	// Pengembalian dana pesanan Bayar Online; nil untuk COD / tidak ada refund.
	Refund *RefundInfo
}

// RefundInfo: status pengembalian dana yang dilihat pelanggan.
type RefundInfo struct {
	Status string // PENDING (menunggu diproses toko) | PARTIAL | COMPLETED
	Amount int64  // yang sudah dikembalikan
}

const (
	RefundPending   = "PENDING"
	RefundPartial   = "PARTIAL"
	RefundCompleted = "COMPLETED"
)

// refundInfo: dari total refund berhasil (payment_refunds) & status pesanan.
// Pesanan online yang batal/gagal tanpa refund = PENDING (toko wajib mengembalikan dana).
func refundInfo(orderStatus string, total, refunded int64) *RefundInfo {
	switch {
	case refunded >= total && refunded > 0:
		return &RefundInfo{Status: RefundCompleted, Amount: refunded}
	case refunded > 0:
		return &RefundInfo{Status: RefundPartial, Amount: refunded}
	case orderStatus == StatusCancelled || orderStatus == StatusFailed:
		return &RefundInfo{Status: RefundPending}
	}
	return nil
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
	CanRate     bool
	Rating      *int // nil = belum dinilai
}

// ListFilter: filter riwayat pesanan user. From/To nil = tanpa batas; rentang [From, To).
type ListFilter struct {
	Statuses []string
	From, To *time.Time
	Oldest   bool // urut created_at ASC (default DESC)
}

type Repository struct{}

func (Repository) StoreByID(ctx context.Context, db database.DBTX, id string) (Store, error) {
	var s Store
	err := db.QueryRow(ctx, `SELECT id, name, address, is_open FROM stores WHERE id = $1`, id).
		Scan(&s.ID, &s.Name, &s.Address, &s.IsOpen)
	return s, err
}

// FreeDeliveryUsed: kupon gratis ongkir terpakai sejak `since`: pesanan yang tidak batal/gagal,
// plus (bila includePending) sesi bayar online PENDING yang memakai kupon.
func (Repository) FreeDeliveryUsed(ctx context.Context, db database.DBTX, userID string, since time.Time, includePending bool) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM orders
		         WHERE user_id = $1 AND free_delivery_coupon AND created_at >= $2
		           AND status NOT IN ('CANCELLED', 'FAILED'))
		     + CASE WHEN $3 THEN (SELECT count(*) FROM payments
		         WHERE user_id = $1 AND status = 'PENDING' AND order_id IS NULL AND created_at >= $2
		           AND (snapshot->'quote'->>'FreeDeliveryCoupon')::boolean IS TRUE) ELSE 0 END`,
		userID, since, includePending).Scan(&n)
	return n, err
}

// LockUser: kunci baris user dalam transaksi (serialisasi checkout per user).
func (Repository) LockUser(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID)
	return err
}

// AddressCoord: alamat milik user? + koordinatnya (boleh kosong).
func (Repository) AddressCoord(ctx context.Context, db database.DBTX, id, userID string) (bool, *float64, *float64, error) {
	var lat, lng *float64
	err := db.QueryRow(ctx, `SELECT lat, lng FROM addresses WHERE id = $1 AND user_id = $2`, id, userID).Scan(&lat, &lng)
	if database.IsNoRows(err) {
		return false, nil, nil, nil
	}
	return err == nil, lat, lng, err
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
	// Pembayaran online (§5.27): pesanan langsung PAID & ditautkan ke payments.
	Status      string  // "" = PENDING
	PaymentID   *string // payments.id
	HistoryNote *string // mis. "Dibayar via GoPay"
}

func (Repository) InsertOrder(ctx context.Context, tx pgx.Tx, in insertOrder) (string, error) {
	q := in.Quote
	status := in.Status
	if status == "" {
		status = StatusPending
	}
	var voucherID *string
	if q.Voucher != nil {
		voucherID = &q.Voucher.ID
	}
	var etaMin, etaMax *int
	if q.ETA != nil {
		etaMin, etaMax = &q.ETA.MinMinutes, &q.ETA.MaxMinutes
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (order_number, user_id, store_id, address_id, subtotal, delivery_fee, discount, total,
		                    voucher_id, payment_method, note, idempotency_key,
		                    payment_account_id, payment_provider, payment_account_masked, status, payment_id,
		                    distance_km, eta_min_minutes, eta_max_minutes, free_delivery_coupon, delivery_fee_waived)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::order_status, $17, $18, $19, $20, $21, $22)
		RETURNING id::text`,
		in.OrderNumber, in.UserID, q.StoreID, q.AddressID, q.Subtotal, q.DeliveryFee, q.Discount, q.Total,
		voucherID, in.Payment.Method, in.Note, in.IdempotencyKey,
		in.Payment.AccountID, in.Payment.Provider, in.Payment.AccountMasked, status, in.PaymentID,
		q.DistanceKM, etaMin, etaMax, q.FreeDeliveryCoupon, q.DeliveryFeeWaived).Scan(&id)
	if err != nil {
		return "", err
	}
	batch := &pgx.Batch{}
	for i, l := range q.Lines {
		var bundle []byte
		if len(l.Bundle) > 0 {
			if bundle, err = json.Marshal(l.Bundle); err != nil {
				return "", err
			}
		}
		batch.Queue(`
			INSERT INTO order_items (order_id, product_id, product_name, option_id, option_name, unit_price, quantity, note, subtotal, sort_order, bundle_items)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			id, l.ProductID, l.ProductName, l.OptionID, l.OptionName, l.UnitPrice, l.Quantity, l.Note, l.Subtotal, i, bundle)
		batch.Queue(`UPDATE products SET sold_count = sold_count + $2 WHERE id = $1`, l.ProductID, l.Quantity)
	}
	batch.Queue(`INSERT INTO order_status_history (order_id, status, note) VALUES ($1, $2::order_status, $3)`, id, status, in.HistoryNote)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return "", err
	}
	// Stok outlet (§5.29). Pesanan yang sudah dibayar online tetap dibuat; stok berhenti di 0.
	newStock := `sp.stock - oi.qty`
	if status != StatusPending {
		newStock = `GREATEST(sp.stock - oi.qty, 0)`
	}
	_, err = tx.Exec(ctx, `
		UPDATE store_products sp SET stock = `+newStock+`, updated_at = now()
		FROM (`+stockUnits+`) oi
		WHERE sp.store_id = $2 AND sp.product_id = oi.product_id AND sp.stock IS NOT NULL`, id, q.StoreID)
	if database.IsCheckViolation(err) {
		// Stok diambil pesanan lain di antara quote dan insert.
		return "", httpx.Unprocessable("OUT_OF_STOCK", "Stok menu di keranjangmu baru saja berkurang. Muat ulang menu lalu coba lagi.")
	}
	return id, err
}

// stockUnits: unit stok yang dipakai pesanan $1 = semua baris (termasuk combo itu sendiri)
// + isi combo dari snapshot bundle_items (jumlah paket × jumlah isi per paket).
const stockUnits = `
	SELECT product_id, sum(qty)::int AS qty FROM (
		SELECT product_id, quantity AS qty FROM order_items WHERE order_id = $1
		UNION ALL
		SELECT b->>'product_id', oi.quantity * (b->>'quantity')::int
		FROM order_items oi, jsonb_array_elements(oi.bundle_items) b
		WHERE oi.order_id = $1 AND oi.bundle_items IS NOT NULL
	) u GROUP BY product_id`

// OrderStatusForUpdate mengunci order milik user; pgx.ErrNoRows bila tidak ada.
func (Repository) OrderStatusForUpdate(ctx context.Context, tx pgx.Tx, id, userID string) (status string, voucherID *string, err error) {
	err = tx.QueryRow(ctx, `SELECT status::text, voucher_id::text FROM orders WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID).
		Scan(&status, &voucherID)
	return status, voucherID, err
}

func (Repository) SetStatus(ctx context.Context, tx pgx.Tx, id, status string, meta StatusMeta) error {
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2::order_status, updated_at = now() WHERE id = $1`, id, status); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status, note, changed_by)
		VALUES ($1, $2::order_status, NULLIF($3, ''), NULLIF($4, '')::uuid)`, id, status, meta.Note, meta.ActorID)
	return err
}

// OrderOwner: user pemilik pesanan; pgx.ErrNoRows bila tidak ada.
func (Repository) OrderOwner(ctx context.Context, db database.DBTX, id string) (string, error) {
	var userID string
	err := db.QueryRow(ctx, `SELECT user_id::text FROM orders WHERE id = $1`, id).Scan(&userID)
	return userID, err
}

// RevertSoldCount dipanggil saat pesanan batal/gagal: sold_count turun & stok outlet kembali.
func (Repository) RevertSoldCount(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE products p SET sold_count = GREATEST(p.sold_count - oi.qty, 0)
		FROM (SELECT product_id, sum(quantity)::int AS qty FROM order_items WHERE order_id = $1 GROUP BY product_id) oi
		WHERE p.id = oi.product_id`, orderID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE store_products sp SET stock = sp.stock + oi.qty, updated_at = now()
		FROM (`+stockUnits+`) oi,
		     orders o
		WHERE o.id = $1 AND sp.store_id = o.store_id AND sp.product_id = oi.product_id AND sp.stock IS NOT NULL`, orderID)
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

func (Repository) List(ctx context.Context, db database.DBTX, userID string, f ListFilter, limit, offset int) ([]Summary, int, error) {
	const where = `o.user_id = $1 AND (cardinality($2::text[]) = 0 OR o.status::text = ANY($2))
		AND ($3::timestamptz IS NULL OR o.created_at >= $3) AND ($4::timestamptz IS NULL OR o.created_at < $4)`
	var total int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM orders o WHERE `+where,
		userID, f.Statuses, f.From, f.To).Scan(&total); err != nil {
		return nil, 0, err
	}
	dir := "DESC"
	if f.Oldest {
		dir = "ASC"
	}
	rows, err := db.Query(ctx, `
		SELECT o.id::text, o.order_number, s.name, o.total, o.status::text, o.created_at,
		       COALESCE(agg.first_item, ''), COALESCE(agg.item_count, 0), COALESCE(agg.qty, 0),
		       o.status = 'COMPLETED' AND r.order_id IS NULL, r.rating
		FROM orders o
		LEFT JOIN order_ratings r ON r.order_id = o.id
		JOIN stores s ON s.id = o.store_id
		LEFT JOIN LATERAL (
			SELECT (array_agg(product_name ORDER BY sort_order))[1] AS first_item,
			       count(*)::int AS item_count, sum(quantity)::int AS qty
			FROM order_items WHERE order_id = o.id
		) agg ON true
		WHERE `+where+`
		ORDER BY o.created_at `+dir+`, o.order_number `+dir+`
		LIMIT $5 OFFSET $6`, userID, f.Statuses, f.From, f.To, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Summary, error) {
		var s Summary
		err := row.Scan(&s.ID, &s.OrderNumber, &s.StoreName, &s.Total, &s.Status, &s.CreatedAt,
			&s.FirstItem, &s.ItemCount, &s.Qty, &s.CanRate, &s.Rating)
		return s, err
	})
	return list, total, err
}

// Get mengembalikan detail lengkap pesanan milik user; pgx.ErrNoRows bila tidak ada.
func (Repository) Get(ctx context.Context, db database.DBTX, id, userID string) (Order, error) {
	var o Order
	var addrID *string
	var etaMin, etaMax *int
	err := db.QueryRow(ctx, `
		SELECT o.id::text, o.order_number, o.user_id::text, o.status::text, o.subtotal, o.delivery_fee, o.discount, o.total,
		       o.payment_method, o.note, o.voucher_id::text, v.code, o.created_at,
		       s.id, s.name, s.address, s.is_open, o.address_id::text,
		       o.payment_account_id::text, o.payment_provider, o.payment_account_masked,
		       o.distance_km::float8, o.eta_min_minutes, o.eta_max_minutes, o.free_delivery_coupon, o.delivery_fee_waived
		FROM orders o
		JOIN stores s ON s.id = o.store_id
		LEFT JOIN vouchers v ON v.id = o.voucher_id
		WHERE o.id = $1 AND o.user_id = $2`, id, userID).
		Scan(&o.ID, &o.OrderNumber, &o.UserID, &o.Status, &o.Subtotal, &o.DeliveryFee, &o.Discount, &o.Total,
			&o.PaymentMethod, &o.Note, &o.VoucherID, &o.VoucherCode, &o.CreatedAt,
			&o.Store.ID, &o.Store.Name, &o.Store.Address, &o.Store.IsOpen, &addrID,
			&o.PaymentAccountID, &o.PaymentProvider, &o.PaymentAccountMasked,
			&o.DistanceKM, &etaMin, &etaMax, &o.FreeDeliveryCoupon, &o.DeliveryFeeWaived)
	if err != nil {
		return o, err
	}
	if etaMin != nil && etaMax != nil {
		o.ETA = &ETA{MinMinutes: *etaMin, MaxMinutes: *etaMax}
	}

	// Bayar Online: uang sudah masuk, jadi pelanggan perlu tahu status pengembalian dananya.
	var refunded *int64
	err = db.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(r.amount) FROM payment_refunds r WHERE r.payment_id = p.id AND r.status = 'SUCCEEDED'), 0)
		FROM payments p WHERE p.order_id = $1`, o.ID).Scan(&refunded)
	if err != nil && !database.IsNoRows(err) {
		return o, err
	}
	if refunded != nil {
		o.Refund = refundInfo(o.Status, o.Total, *refunded)
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
		SELECT product_id, product_name, option_id, option_name, unit_price, quantity, note, subtotal, bundle_items
		FROM order_items WHERE order_id = $1 ORDER BY sort_order`, o.ID)
	if err != nil {
		return o, err
	}
	o.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Line, error) {
		var l Line
		var bundle []byte
		err := row.Scan(&l.ProductID, &l.ProductName, &l.OptionID, &l.OptionName, &l.UnitPrice, &l.Quantity, &l.Note, &l.Subtotal, &bundle)
		if err == nil && bundle != nil {
			err = json.Unmarshal(bundle, &l.Bundle)
		}
		return l, err
	})
	if err != nil {
		return o, err
	}

	rows, err = db.Query(ctx, `
		SELECT h.status::text, h.at, h.note, u.name
		FROM order_status_history h LEFT JOIN users u ON u.id = h.changed_by
		WHERE h.order_id = $1 ORDER BY h.at, h.status`, o.ID)
	if err != nil {
		return o, err
	}
	o.History, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatusEvent, error) {
		var e StatusEvent
		err := row.Scan(&e.Status, &e.At, &e.Note, &e.ChangedBy)
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
