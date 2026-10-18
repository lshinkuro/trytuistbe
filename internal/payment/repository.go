package payment

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/order"
)

const (
	StatusPending   = "PENDING"
	StatusPaid      = "PAID"
	StatusFailed    = "FAILED"
	StatusExpired   = "EXPIRED"
	StatusCancelled = "CANCELLED"
)

// Payment: satu sesi pembayaran (tabel payments).
type Payment struct {
	ID              string
	UserID          string
	StoreID         string
	StoreName       string
	ProviderOrderID string
	Status          string
	Amount          int64
	Snapshot        Snapshot
	RequestHash     string
	SnapToken       *string
	RedirectURL     *string
	PaymentType     *string
	ProviderTxnID   *string
	ProviderStatus  *string
	StatusMessage   *string
	RefundStatus    *string
	ExpiresAt       time.Time
	PaidAt          *time.Time
	LastCheckedAt   *time.Time
	OrderID         *string
	OrderNumber     *string
	OrderStatus     *string
	CreatedAt       time.Time
}

// Snapshot: isi pesanan saat checkout; dipakai membuat pesanan saat lunas.
type Snapshot struct {
	Quote order.Quote `json:"quote"`
	Note  *string     `json:"note"`
}

type repository struct{}

const paymentColumns = `
	p.id::text, p.user_id::text, p.store_id, s.name, p.provider_order_id, p.status::text, p.amount, p.snapshot,
	p.request_hash, p.snap_token, p.redirect_url, p.payment_type, p.provider_txn_id, p.provider_status,
	p.status_message, p.refund_status, p.expires_at, p.paid_at, p.last_checked_at,
	p.order_id::text, o.order_number, o.status::text, p.created_at`

const paymentFrom = `
	FROM payments p
	JOIN stores s ON s.id = p.store_id
	LEFT JOIN orders o ON o.id = p.order_id`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	var snap []byte
	err := row.Scan(&p.ID, &p.UserID, &p.StoreID, &p.StoreName, &p.ProviderOrderID, &p.Status, &p.Amount, &snap,
		&p.RequestHash, &p.SnapToken, &p.RedirectURL, &p.PaymentType, &p.ProviderTxnID, &p.ProviderStatus,
		&p.StatusMessage, &p.RefundStatus, &p.ExpiresAt, &p.PaidAt, &p.LastCheckedAt,
		&p.OrderID, &p.OrderNumber, &p.OrderStatus, &p.CreatedAt)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(snap, &p.Snapshot)
	return p, err
}

func (repository) Get(ctx context.Context, db database.DBTX, id string) (Payment, error) {
	return scanPayment(db.QueryRow(ctx, `SELECT `+paymentColumns+paymentFrom+` WHERE p.id = $1`, id))
}

// Lock: kunci baris payments (FOR UPDATE OF p) di dalam transaksi.
func (repository) Lock(ctx context.Context, tx pgx.Tx, id string) (Payment, error) {
	return scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+paymentFrom+` WHERE p.id = $1 FOR UPDATE OF p`, id))
}

func (repository) IDByProviderOrderID(ctx context.Context, db database.DBTX, providerOrderID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id::text FROM payments WHERE provider_order_id = $1`, providerOrderID).Scan(&id)
	return id, err
}

func (repository) IDByIdempotencyKey(ctx context.Context, db database.DBTX, userID, key string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id::text FROM payments WHERE user_id = $1 AND idempotency_key = $2`, userID, key).Scan(&id)
	return id, err
}

func (repository) List(ctx context.Context, db database.DBTX, userID string, statuses []string, limit int) ([]Payment, error) {
	rows, err := db.Query(ctx, `SELECT `+paymentColumns+paymentFrom+`
		WHERE p.user_id = $1 AND (cardinality($2::text[]) = 0 OR p.status::text = ANY($2))
		ORDER BY p.created_at DESC LIMIT $3`, userID, statuses, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
}

func (repository) PendingIDs(ctx context.Context, db database.DBTX, userID string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT id::text FROM payments WHERE user_id = $1 AND status = 'PENDING'`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// OverdueIDs: sesi PENDING yang sudah lewat expires_at + grace (untuk sweeper).
func (repository) OverdueIDs(ctx context.Context, db database.DBTX, before time.Time, limit int) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT id::text FROM payments WHERE status = 'PENDING' AND expires_at < $1 ORDER BY expires_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

type newPayment struct {
	UserID, StoreID, ProviderOrderID, RequestHash string
	Amount                                        int64
	Snapshot                                      Snapshot
	IdempotencyKey                                *string
	ExpiresAt                                     time.Time
}

func (repository) Insert(ctx context.Context, db database.DBTX, in newPayment) (string, error) {
	snap, err := json.Marshal(in.Snapshot)
	if err != nil {
		return "", err
	}
	var id string
	err = db.QueryRow(ctx, `
		INSERT INTO payments (user_id, store_id, provider_order_id, amount, snapshot, request_hash, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		in.UserID, in.StoreID, in.ProviderOrderID, in.Amount, snap, in.RequestHash, in.IdempotencyKey, in.ExpiresAt).Scan(&id)
	return id, err
}

func (repository) SetSnap(ctx context.Context, db database.DBTX, id, token, redirectURL string) error {
	_, err := db.Exec(ctx, `UPDATE payments SET snap_token = $2, redirect_url = $3, updated_at = now() WHERE id = $1`, id, token, redirectURL)
	return err
}

// update: tulis kolom status & info provider (NULL = biarkan nilai lama).
type update struct {
	Status         string
	PaymentType    string
	ProviderTxnID  string
	ProviderStatus string
	StatusMessage  *string
	RefundStatus   string
	PaidAt         *time.Time
	OrderID        *string
	Checked        bool
}

func (repository) Update(ctx context.Context, db database.DBTX, id string, u update) error {
	_, err := db.Exec(ctx, `
		UPDATE payments SET
			status          = COALESCE(NULLIF($2, '')::payment_status, status),
			payment_type    = COALESCE(NULLIF($3, ''), payment_type),
			provider_txn_id = COALESCE(NULLIF($4, ''), provider_txn_id),
			provider_status = COALESCE(NULLIF($5, ''), provider_status),
			status_message  = COALESCE($6, status_message),
			refund_status   = COALESCE(NULLIF($7, ''), refund_status),
			paid_at         = COALESCE($8, paid_at),
			order_id        = COALESCE($9::uuid, order_id),
			last_checked_at = CASE WHEN $10 THEN now() ELSE last_checked_at END,
			updated_at      = now()
		WHERE id = $1`,
		id, u.Status, u.PaymentType, u.ProviderTxnID, u.ProviderStatus, u.StatusMessage, u.RefundStatus, u.PaidAt, u.OrderID, u.Checked)
	return err
}

func (repository) TouchChecked(ctx context.Context, db database.DBTX, id string) error {
	_, err := db.Exec(ctx, `UPDATE payments SET last_checked_at = now() WHERE id = $1`, id)
	return err
}

func (repository) InsertEvent(ctx context.Context, db database.DBTX, paymentID *string, source, providerStatus string, signatureOK *bool, payload []byte) error {
	if !json.Valid(payload) {
		b, _ := json.Marshal(map[string]string{"raw": string(payload)})
		payload = b
	}
	_, err := db.Exec(ctx, `INSERT INTO payment_events (payment_id, source, provider_status, signature_ok, payload)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)`, paymentID, source, providerStatus, signatureOK, payload)
	return err
}

// Customer: data pelanggan untuk customer_details Snap.
func (repository) Customer(ctx context.Context, db database.DBTX, userID string) (name, email, phone string, err error) {
	var e, p *string
	err = db.QueryRow(ctx, `SELECT name, email, phone FROM users WHERE id = $1`, userID).Scan(&name, &e, &p)
	if e != nil {
		email = *e
	}
	if p != nil {
		phone = *p
	}
	return
}
