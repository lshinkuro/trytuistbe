package order

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// ===== Pembayaran online (§5.27) =====
// Checkout online: Prepare (validasi + quote, tanpa menyimpan) → sesi Midtrans (paket payment)
// → saat lunas CreatePaidOrder membuat pesanan PAID dari snapshot quote.

// IsOnlineMethod: metode yang dibayar lewat Midtrans (bukan tunai ke kurir).
func IsOnlineMethod(method string) bool {
	return method == "ONLINE" || method == "QRIS" || method == "EWALLET"
}

// Prepared: hasil validasi checkout sebelum dibayar.
type Prepared struct {
	Quote  Quote
	Method string // COD | ONLINE (QRIS & e-wallet dibaca ONLINE bila Midtrans aktif)
	Note   *string
}

// OnlineEnabled: Midtrans aktif.
func (s *Service) OnlineEnabled() bool { return s.cfg.OnlinePayments }

// Prepare: metode bayar + quote (alamat kosong → alamat utama), sama persis dengan Create tapi tidak menyimpan.
func (s *Service) Prepare(ctx context.Context, userID string, in CreateInput) (Prepared, error) {
	payment, err := s.resolvePayment(ctx, s.pool, userID, in.PaymentMethod, in.PaymentMethodID)
	if err != nil {
		return Prepared{}, err
	}
	method := payment.Method
	if IsOnlineMethod(method) {
		method = "ONLINE"
	}
	if err := s.checkPaymentMethod(ctx, method); err != nil {
		return Prepared{}, err
	}
	quoteInput := in.QuoteInput
	quoteInput.skipPendingCoupons = method == "ONLINE"
	if strings.TrimSpace(quoteInput.AddressID) == "" {
		id, err := s.repo.DefaultAddressID(ctx, s.pool, userID)
		if err != nil && !database.IsNoRows(err) {
			return Prepared{}, err
		}
		quoteInput.AddressID = id
	}
	q, err := s.quote(ctx, s.pool, userID, quoteInput)
	if err != nil {
		return Prepared{}, err
	}
	var note *string
	if n := strings.TrimSpace(in.Note); n != "" {
		note = &n
	}
	return Prepared{Quote: q, Method: method, Note: note}, nil
}

// CreatePaidOrder: dipanggil paket payment di dalam transaksinya saat pembayaran lunas.
// Harga tidak dihitung ulang dan stok/jam buka tidak dicek ulang: uang sudah diterima.
// Voucher yang ternyata sudah terpakai di pesanan lain tetap dihormati (harga sudah dibayar).
func (s *Service) CreatePaidOrder(ctx context.Context, tx pgx.Tx, userID, paymentID string, q Quote, note *string, provider, historyNote string) (string, string, error) {
	number, err := s.repo.NextOrderNumber(ctx, tx, s.now().In(s.cfg.Location))
	if err != nil {
		return "", "", err
	}
	var prov, hist *string
	if provider != "" {
		p := strings.ToUpper(provider)
		prov = &p
	}
	if historyNote != "" {
		hist = &historyNote
	}
	pid := paymentID
	orderID, err := s.repo.InsertOrder(ctx, tx, insertOrder{
		OrderNumber: number, UserID: userID, Quote: q,
		PaymentMethod: "ONLINE", Payment: PaymentChoice{Method: "ONLINE", Provider: prov},
		Note: note, Status: StatusPaid, PaymentID: &pid, HistoryNote: hist,
	})
	if err != nil {
		return "", "", err
	}
	if q.Voucher != nil {
		if _, err := s.repo.MarkVoucherUsed(ctx, tx, userID, q.Voucher.ID); err != nil {
			return "", "", err
		}
	}
	return orderID, number, nil
}

// QuoteDTOOf: bentuk JSON quote (§5.10) untuk response checkout.
func QuoteDTOOf(q Quote) QuoteDTO { return toQuoteDTO(q) }

// ErrPaymentRequired dipakai paket lain untuk pesan yang sama.
var ErrPaymentRequired = httpx.Unprocessable("PAYMENT_REQUIRED", "Pesanan dengan pembayaran online dibuat setelah dibayar. Gunakan /v1/checkout.")

// DecodeCreate: body POST /v1/orders → CreateInput (dipakai juga POST /v1/checkout).
func DecodeCreate(r *http.Request) (CreateInput, error) {
	key, err := httpx.IdempotencyKey(r)
	if err != nil {
		return CreateInput{}, err
	}
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return CreateInput{}, err
	}
	return CreateInput{
		QuoteInput:      req.toInput(),
		PaymentMethod:   req.PaymentMethod,
		PaymentMethodID: req.PaymentMethodID,
		Note:            req.Note,
		IdempotencyKey:  key,
	}, nil
}
