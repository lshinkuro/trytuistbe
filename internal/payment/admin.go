package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// Bagian web admin (fase 5b): detail pembayaran, riwayat event, dan refund.

const (
	RefundMidtrans = "MIDTRANS"
	RefundManual   = "MANUAL"

	refundPending   = "PENDING"
	refundSucceeded = "SUCCEEDED"
	refundFailed    = "FAILED"
)

type Refund struct {
	ID        string
	Amount    int64
	Reason    string
	Method    string
	Status    string
	Message   *string
	AdminName *string
	CreatedAt time.Time
}

type Event struct {
	Source         string
	ProviderStatus *string
	SignatureOK    *bool
	Payload        json.RawMessage
	CreatedAt      time.Time
}

type RefundInput struct {
	Amount int64 // 0 = seluruh sisa
	Reason string
	Manual bool // dana sudah dikembalikan di luar Midtrans; hanya dicatat
}

var (
	errNotPaid         = httpx.Conflict("PAYMENT_NOT_PAID", "Hanya pembayaran yang sudah lunas yang bisa direfund.")
	errNoGatewayRefund = httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE",
		"Midtrans belum aktif. Kembalikan dana secara manual lalu catat sebagai refund manual.")
	errRefundNotSupported = httpx.Unprocessable("REFUND_NOT_SUPPORTED",
		"Pembayaran lewat transfer bank / gerai tidak bisa direfund otomatis. Transfer balik ke pelanggan, lalu catat sebagai refund manual.")
)

// RefundableOnline: channel yang bisa direfund lewat Refund API Midtrans.
// VA (bank_transfer, echannel) dan gerai (cstore) tidak bisa.
func RefundableOnline(paymentType *string) bool {
	if paymentType == nil {
		return false
	}
	switch *paymentType {
	case "bank_transfer", "echannel", "cstore":
		return false
	}
	return true
}

// AdminGet: pembayaran milik siapa pun (web admin).
func (s *Service) AdminGet(ctx context.Context, id string) (Payment, error) {
	if !uuidRe.MatchString(id) {
		return Payment{}, errPaymentNotFound
	}
	p, err := s.repo.Get(ctx, s.pool, id)
	if database.IsNoRows(err) {
		return Payment{}, errPaymentNotFound
	}
	return p, err
}

// DTO: bentuk JSON sama dengan GET /v1/payments/{id}.
func (s *Service) DTO(p Payment) PaymentDTO { return s.toDTO(p) }

func (s *Service) Refunds(ctx context.Context, paymentID string) ([]Refund, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.amount, r.reason, r.method, r.status, r.message, u.name, r.created_at
		FROM payment_refunds r LEFT JOIN users u ON u.id = r.admin_id
		WHERE r.payment_id = $1 ORDER BY r.created_at`, paymentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Refund, error) {
		var r Refund
		err := row.Scan(&r.ID, &r.Amount, &r.Reason, &r.Method, &r.Status, &r.Message, &r.AdminName, &r.CreatedAt)
		return r, err
	})
}

func (s *Service) Events(ctx context.Context, paymentID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, provider_status, signature_ok, payload, created_at
		FROM payment_events WHERE payment_id = $1 ORDER BY created_at, id`, paymentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Source, &e.ProviderStatus, &e.SignatureOK, &e.Payload, &e.CreatedAt)
		return e, err
	})
}

// refundTotals: (terpakai = PENDING + SUCCEEDED, berhasil = SUCCEEDED).
func refundTotals(ctx context.Context, db database.DBTX, paymentID string) (reserved, succeeded int64, err error) {
	err = db.QueryRow(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE status <> 'FAILED'), 0),
		       COALESCE(sum(amount) FILTER (WHERE status = 'SUCCEEDED'), 0)
		FROM payment_refunds WHERE payment_id = $1`, paymentID).Scan(&reserved, &succeeded)
	return
}

// RefundedAmount: total refund yang berhasil.
func (s *Service) RefundedAmount(ctx context.Context, paymentID string) (int64, error) {
	_, done, err := refundTotals(ctx, s.pool, paymentID)
	return done, err
}

// Refund: kembalikan dana penuh / sebagian. Pesanan tidak diubah otomatis (§5.27.4): admin yang memutuskan.
//
// Baris refund ditulis PENDING dulu (di bawah lock payments) sebelum memanggil Midtrans, jadi
// dua klik bersamaan tidak bisa melebihi total. id refund dipakai sebagai refund_key.
func (s *Service) Refund(ctx context.Context, adminID, id string, in RefundInput) (Refund, error) {
	p, err := s.AdminGet(ctx, id)
	if err != nil {
		return Refund{}, err
	}
	if p.Status != StatusPaid {
		return Refund{}, errNotPaid
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return Refund{}, httpx.Validation("Alasan refund wajib diisi.", httpx.FieldDetail{Field: "reason", Message: "wajib diisi"})
	}
	method := RefundManual
	if !in.Manual {
		if !s.Enabled() {
			return Refund{}, errNoGatewayRefund
		}
		if !RefundableOnline(p.PaymentType) {
			return Refund{}, errRefundNotSupported
		}
		method = RefundMidtrans
	}

	var r Refund
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusPaid {
			return errNotPaid
		}
		reserved, done, err := refundTotals(ctx, tx, id)
		if err != nil {
			return err
		}
		left := cur.Amount - reserved
		amount := in.Amount
		if amount == 0 {
			amount = left
		}
		if amount <= 0 || amount > left {
			return httpx.Unprocessable("REFUND_AMOUNT_INVALID", "Jumlah refund harus Rp 1 sampai sisa "+rupiah(max(left, 0))+".")
		}
		status := refundPending
		if method == RefundManual {
			status = refundSucceeded
		}
		r = Refund{Amount: amount, Reason: in.Reason, Method: method, Status: status}
		if err := tx.QueryRow(ctx, `
			INSERT INTO payment_refunds (payment_id, amount, reason, method, status, admin_id)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid) RETURNING id::text, created_at`,
			id, amount, in.Reason, method, status, adminID).Scan(&r.ID, &r.CreatedAt); err != nil {
			return err
		}
		if method == RefundManual {
			return s.repo.Update(ctx, tx, id, update{RefundStatus: refundLabel(done+amount, cur.Amount)})
		}
		return nil
	})
	if err != nil || method == RefundManual {
		return r, err
	}

	gwErr := s.gw.Refund(ctx, p.ProviderOrderID, RefundRequest{RefundKey: r.ID, Amount: r.Amount, Reason: r.Reason})
	var msg *string
	r.Status = refundSucceeded
	// Timeout: hasil di Midtrans tidak pasti. Ditandai FAILED; bila ternyata berhasil, webhook "refund"
	// tetap mengisi refund_status, dan Midtrans sendiri menolak total refund yang melebihi pembayaran.
	if gwErr != nil {
		m := gwErr.Error()
		r.Status, msg, r.Message = refundFailed, &m, &m
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_refunds SET status = $2, message = $3, updated_at = now() WHERE id = $1`,
			r.ID, r.Status, msg); err != nil {
			return err
		}
		if r.Status != refundSucceeded {
			return nil
		}
		cur, err := s.repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		_, done, err := refundTotals(ctx, tx, id)
		if err != nil {
			return err
		}
		return s.repo.Update(ctx, tx, id, update{RefundStatus: refundLabel(done, cur.Amount)})
	})
	if err != nil {
		return r, err
	}
	var ge *GatewayError
	switch {
	case gwErr == nil:
		return r, nil
	case errors.As(gwErr, &ge):
		return r, &httpx.AppError{Status: http.StatusUnprocessableEntity, Type: "REFUND_REJECTED",
			Message: "Midtrans menolak refund: " + ge.Messages}
	default:
		return r, errGateway
	}
}

func refundLabel(refunded, amount int64) string {
	if refunded >= amount {
		return "REFUND"
	}
	return "PARTIAL_REFUND"
}
