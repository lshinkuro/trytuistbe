package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
	"trytuist-api/internal/push"
)

type Config struct {
	Env             string        // dev | staging | prod → bagian dari order_id Midtrans
	Expiry          time.Duration // batas bayar (default 15 menit)
	PublicBaseURL   string        // untuk finish URL Snap
	EnabledPayments []string      // kosong = semua channel aktif di dashboard
	Location        *time.Location
	// CheckInterval: jeda minimal antar cek GET Status per sesi saat iOS polling (default 5 detik).
	CheckInterval time.Duration
	// Notifier: push "Pembayaran berhasil" / "Waktu pembayaran habis" (§5.28). nil = nonaktif.
	Notifier push.Notifier
}

// MinAmount: total minimum transaksi online (Rp).
const MinAmount = 1000

type Service struct {
	pool   *pgxpool.Pool
	orders *order.Service
	gw     Gateway
	cfg    Config
	repo   repository
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, orders *order.Service, gw Gateway, cfg Config) *Service {
	if cfg.Expiry <= 0 {
		cfg.Expiry = 15 * time.Minute
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 5 * time.Second
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	return &Service{pool: pool, orders: orders, gw: gw, cfg: cfg, now: time.Now}
}

// Enabled: Midtrans terpasang.
func (s *Service) Enabled() bool { return s.gw != nil }

func (s *Service) FinishURL() string { return s.cfg.PublicBaseURL + "/v1/payments/finish" }

var (
	errPaymentNotFound = httpx.NotFound("Pembayaran tidak ditemukan.")
	errGateway         = httpx.NewError(http.StatusBadGateway, "PAYMENT_GATEWAY_ERROR", "Pembayaran sedang gangguan. Coba lagi sebentar lagi.")
	uuidRe             = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// CheckoutResult: COD → Order; online → Payment.
type CheckoutResult struct {
	Order   *order.Order
	Payment *Payment
	Created bool // false = sesi lama dikembalikan (Idempotency-Key sama)
}

// Checkout (POST /v1/checkout).
func (s *Service) Checkout(ctx context.Context, userID string, in order.CreateInput) (CheckoutResult, error) {
	if !s.Enabled() {
		// Midtrans belum dipasang: perilaku lama, pesanan langsung dibuat.
		o, err := s.orders.Create(ctx, userID, in)
		if err != nil {
			return CheckoutResult{}, err
		}
		return CheckoutResult{Order: &o, Created: true}, nil
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if len(key) > 100 {
		return CheckoutResult{}, httpx.Validation("Header Idempotency-Key terlalu panjang (maks. 100).")
	}
	prep, err := s.orders.Prepare(ctx, userID, in)
	if err != nil {
		return CheckoutResult{}, err
	}
	if prep.Method != "ONLINE" {
		in.PaymentMethod, in.PaymentMethodID = prep.Method, prep.Method
		o, err := s.orders.Create(ctx, userID, in)
		if err != nil {
			return CheckoutResult{}, err
		}
		return CheckoutResult{Order: &o, Created: true}, nil
	}
	q := prep.Quote
	if q.Total < MinAmount {
		return CheckoutResult{}, httpx.Unprocessable("AMOUNT_TOO_SMALL", fmt.Sprintf("Minimal pembayaran online Rp %d.", MinAmount))
	}
	hash := requestHash(q, prep.Note)

	var idemKey *string
	if key != "" {
		idemKey = &key
		if p, found, err := s.byIdempotencyKey(ctx, userID, key, hash); found || err != nil {
			return CheckoutResult{Payment: p}, err
		}
	}

	// Satu sesi aktif per user: sesi PENDING lama dihentikan supaya tidak terbayar dua kali.
	if err := s.cancelPending(ctx, userID); err != nil {
		return CheckoutResult{}, err
	}

	now := s.now()
	expires := now.Add(s.cfg.Expiry)
	providerOrderID, err := s.newProviderOrderID(now)
	if err != nil {
		return CheckoutResult{}, err
	}
	id, err := s.repo.Insert(ctx, s.pool, newPayment{
		UserID: userID, StoreID: q.StoreID, ProviderOrderID: providerOrderID, RequestHash: hash,
		Amount: q.Total, Snapshot: Snapshot{Quote: q, Note: prep.Note}, IdempotencyKey: idemKey, ExpiresAt: expires,
	})
	if err != nil {
		if idemKey != nil && database.IsUniqueViolation(err, "payments_idempotency") {
			p, _, err := s.byIdempotencyKey(ctx, userID, key, hash)
			return CheckoutResult{Payment: p}, err
		}
		return CheckoutResult{}, err
	}

	req, err := s.snapRequest(ctx, userID, id, providerOrderID, q, now)
	if err != nil {
		return CheckoutResult{}, err
	}
	snapCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := s.gw.CreateSnap(snapCtx, req)
	if err != nil {
		slog.ErrorContext(ctx, "midtrans: buat transaksi Snap gagal", "payment_id", id, "err", err)
		msg := "Gagal membuat transaksi pembayaran."
		_ = s.repo.Update(ctx, s.pool, id, update{Status: StatusFailed, StatusMessage: &msg})
		return CheckoutResult{}, errGateway
	}
	if err := s.repo.SetSnap(ctx, s.pool, id, snap.Token, snap.RedirectURL); err != nil {
		return CheckoutResult{}, err
	}
	p, err := s.repo.Get(ctx, s.pool, id)
	if err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{Payment: &p, Created: true}, nil
}

func (s *Service) byIdempotencyKey(ctx context.Context, userID, key, hash string) (*Payment, bool, error) {
	id, err := s.repo.IDByIdempotencyKey(ctx, s.pool, userID, key)
	if database.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	p, err := s.repo.Get(ctx, s.pool, id)
	if err != nil {
		return nil, true, err
	}
	if p.RequestHash != hash {
		return nil, true, httpx.NewError(http.StatusConflict, "IDEMPOTENCY_CONFLICT",
			"Idempotency-Key ini sudah dipakai untuk isi keranjang yang berbeda.")
	}
	return &p, true, nil
}

// requestHash: sidik isi pesanan (outlet, alamat, item, voucher, total, catatan).
func requestHash(q order.Quote, note *string) string {
	type line struct {
		P, O string
		Q    int
		N    string
	}
	v := struct {
		Store, Addr, Voucher string
		Total                int64
		Lines                []line
		Note                 string
	}{Store: q.StoreID, Total: q.Total}
	if q.AddressID != nil {
		v.Addr = *q.AddressID
	}
	if q.Voucher != nil {
		v.Voucher = q.Voucher.Code
	}
	if note != nil {
		v.Note = *note
	}
	for _, l := range q.Lines {
		o := ""
		if l.OptionID != nil {
			o = *l.OptionID
		}
		v.Lines = append(v.Lines, line{l.ProductID, o, l.Quantity, l.Note})
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const idAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// newProviderOrderID: TT-DEV-20261018-7K3QX9PA. Acak (bukan nomor urut) karena Midtrans menolak
// order_id yang pernah dipakai, termasuk setelah DB dev di-reset.
func (s *Service) newProviderOrderID(now time.Time) (string, error) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(idAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(idAlphabet[n.Int64()])
	}
	env := strings.ToUpper(s.cfg.Env)
	if env == "" {
		env = "DEV"
	}
	return fmt.Sprintf("TT-%s-%s-%s", env, now.In(s.cfg.Location).Format("20060102"), b.String()), nil
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func (s *Service) snapRequest(ctx context.Context, userID, paymentID, providerOrderID string, q order.Quote, now time.Time) (SnapRequest, error) {
	var req SnapRequest
	req.TransactionDetails.OrderID = providerOrderID
	req.TransactionDetails.GrossAmount = q.Total

	var sum int64
	for _, l := range q.Lines {
		id := l.ProductID
		name := l.ProductName
		if l.OptionID != nil {
			id += "-" + *l.OptionID
		}
		if l.OptionName != nil {
			name += " (" + *l.OptionName + ")"
		}
		req.ItemDetails = append(req.ItemDetails, SnapItem{ID: truncate(id, 50), Name: truncate(name, 50), Price: l.UnitPrice, Quantity: l.Quantity})
		sum += l.UnitPrice * int64(l.Quantity)
	}
	if q.DeliveryFee > 0 {
		req.ItemDetails = append(req.ItemDetails, SnapItem{ID: "DELIVERY", Name: "Ongkos kirim", Price: q.DeliveryFee, Quantity: 1})
		sum += q.DeliveryFee
	}
	if q.Discount > 0 {
		name := "Diskon"
		if q.Voucher != nil {
			name = truncate("Voucher "+q.Voucher.Code, 50)
		}
		req.ItemDetails = append(req.ItemDetails, SnapItem{ID: "DISCOUNT", Name: name, Price: -q.Discount, Quantity: 1})
		sum -= q.Discount
	}
	// gross_amount wajib sama dengan jumlah item_details; bila tidak, satu baris total saja.
	if sum != q.Total {
		req.ItemDetails = []SnapItem{{ID: "ORDER", Name: "Pesanan TryTuist", Price: q.Total, Quantity: 1}}
	}

	name, email, phone, err := s.repo.Customer(ctx, s.pool, userID)
	if err != nil {
		return req, err
	}
	req.CustomerDetails = &SnapCustomer{FirstName: truncate(name, 50), Email: email, Phone: phone}
	req.Expiry = &SnapExpiry{
		StartTime: now.In(s.cfg.Location).Format("2006-01-02 15:04:05 -0700"),
		Unit:      "minutes", Duration: int(s.cfg.Expiry / time.Minute),
	}
	if req.Expiry.Duration < 1 {
		req.Expiry.Duration = 1
	}
	if s.cfg.PublicBaseURL != "" {
		req.Callbacks = &struct {
			Finish string `json:"finish"`
		}{Finish: s.FinishURL()}
	}
	req.EnabledPayments = s.cfg.EnabledPayments
	req.CustomField1 = paymentID
	return req, nil
}

// ===== Baca =====

// Get (GET /v1/payments/{id}): bila masih PENDING, server ikut cek ke Midtrans (paling sering
// tiap CheckInterval), supaya alur tetap selesai walau webhook belum tersambung (dev).
func (s *Service) Get(ctx context.Context, userID, id string) (Payment, error) {
	p, err := s.owned(ctx, userID, id)
	if err != nil {
		return p, err
	}
	if p.Status == StatusPending && s.Enabled() &&
		(p.LastCheckedAt == nil || s.now().Sub(*p.LastCheckedAt) >= s.cfg.CheckInterval) {
		if err := s.refresh(ctx, p.ID, "client"); err != nil {
			slog.WarnContext(ctx, "midtrans: cek status gagal", "payment_id", p.ID, "err", err)
		}
		return s.repo.Get(ctx, s.pool, id)
	}
	return p, nil
}

func (s *Service) owned(ctx context.Context, userID, id string) (Payment, error) {
	if !uuidRe.MatchString(id) {
		return Payment{}, errPaymentNotFound
	}
	p, err := s.repo.Get(ctx, s.pool, id)
	if database.IsNoRows(err) || (err == nil && p.UserID != userID) {
		return Payment{}, errPaymentNotFound
	}
	return p, err
}

func (s *Service) List(ctx context.Context, userID string, statuses []string) ([]Payment, error) {
	if statuses == nil {
		statuses = []string{}
	}
	list, err := s.repo.List(ctx, s.pool, userID, statuses, 20)
	if err != nil {
		return nil, err
	}
	// Sesi PENDING yang sudah lewat batas waktu tidak ditampilkan sebagai "menunggu".
	out := list[:0]
	for _, p := range list {
		if p.Status == StatusPending && s.now().After(p.ExpiresAt) {
			if fresh, err := s.Get(ctx, userID, p.ID); err == nil {
				p = fresh
			}
			if p.Status == StatusPending && len(statuses) > 0 {
				continue
			}
		}
		if len(statuses) > 0 && !contains(statuses, p.Status) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ===== Batal & kedaluwarsa =====

// Cancel (POST /v1/payments/{id}/cancel).
func (s *Service) Cancel(ctx context.Context, userID, id string) (Payment, error) {
	p, err := s.owned(ctx, userID, id)
	if err != nil {
		return p, err
	}
	if p.Status != StatusPending {
		return p, httpx.NewError(http.StatusConflict, "PAYMENT_ALREADY_FINAL", "Pembayaran ini sudah "+statusLabel(p.Status)+".")
	}
	if err := s.stop(ctx, p, StatusCancelled, "Dibatalkan oleh pelanggan.", "client"); err != nil {
		return p, err
	}
	p, err = s.repo.Get(ctx, s.pool, id)
	if err == nil && p.Status == StatusPaid {
		return p, httpx.NewError(http.StatusConflict, "PAYMENT_ALREADY_FINAL", "Pembayaran ini sudah lunas.")
	}
	return p, err
}

// cancelPending: hentikan semua sesi PENDING milik user (sebelum checkout baru).
func (s *Service) cancelPending(ctx context.Context, userID string) error {
	ids, err := s.repo.PendingIDs(ctx, s.pool, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		p, err := s.repo.Get(ctx, s.pool, id)
		if err != nil {
			return err
		}
		if err := s.stop(ctx, p, StatusCancelled, "Diganti checkout baru.", "client"); err != nil {
			return err
		}
	}
	return nil
}

// stop: expire ke Midtrans lalu tandai final. Bila ternyata sudah dibayar, status mengikuti Midtrans.
func (s *Service) stop(ctx context.Context, p Payment, to, message, source string) error {
	if p.SnapToken != nil {
		err := s.gw.Expire(ctx, p.ProviderOrderID)
		if err != nil && !errors.Is(err, ErrTransactionNotFound) {
			// Mungkin sudah lunas / sudah expire: ikuti status Midtrans.
			if rerr := s.refresh(ctx, p.ID, source); rerr != nil {
				slog.WarnContext(ctx, "midtrans: expire gagal", "payment_id", p.ID, "err", err)
				return errGateway
			}
		}
	}
	changed := false
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.repo.Lock(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if cur.Status != StatusPending {
			return nil
		}
		changed = true
		return s.repo.Update(ctx, tx, p.ID, update{Status: to, StatusMessage: &message})
	})
	if err == nil && changed && to == StatusExpired {
		s.expiredNotice(p)()
	}
	return err
}

// Sweep: sesi PENDING yang lewat batas + 2 menit → cek Midtrans → EXPIRED bila belum dibayar.
func (s *Service) Sweep(ctx context.Context) error {
	ids, err := s.repo.OverdueIDs(ctx, s.pool, s.now().Add(-2*time.Minute), 50)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.refresh(ctx, id, "sweeper"); err != nil {
			slog.WarnContext(ctx, "sweeper: cek status gagal", "payment_id", id, "err", err)
			continue
		}
		p, err := s.repo.Get(ctx, s.pool, id)
		if err != nil || p.Status != StatusPending {
			continue
		}
		if err := s.stop(ctx, p, StatusExpired, "Batas waktu pembayaran habis.", "sweeper"); err != nil {
			slog.WarnContext(ctx, "sweeper: expire gagal", "payment_id", id, "err", err)
		}
	}
	return nil
}

// RunSweeper: jalan tiap interval sampai ctx selesai.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	if !s.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				slog.Error("sweeper pembayaran", "err", err)
			}
		}
	}
}

// ===== Status dari Midtrans =====

// refresh: GET Status ke Midtrans lalu terapkan.
func (s *Service) refresh(ctx context.Context, id, source string) error {
	p, err := s.repo.Get(ctx, s.pool, id)
	if err != nil {
		return err
	}
	stCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.gw.Status(stCtx, p.ProviderOrderID)
	if errors.Is(err, ErrTransactionNotFound) {
		// User belum memilih metode bayar di Snap.
		if p.Status == StatusPending && s.now().After(p.ExpiresAt) {
			msg := "Batas waktu pembayaran habis."
			changed := false
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				cur, err := s.repo.Lock(ctx, tx, id)
				if err != nil || cur.Status != StatusPending {
					return err
				}
				changed = true
				return s.repo.Update(ctx, tx, id, update{Status: StatusExpired, StatusMessage: &msg, Checked: true})
			})
			if err == nil && changed {
				s.expiredNotice(p)()
			}
			return err
		}
		return s.repo.TouchChecked(ctx, s.pool, id)
	}
	if err != nil {
		_ = s.repo.TouchChecked(ctx, s.pool, id)
		return err
	}
	raw, _ := json.Marshal(st)
	return s.apply(ctx, id, st, source, raw, nil)
}

// HandleNotification (webhook). Mengembalikan errSignature bila tanda tangan salah.
func (s *Service) HandleNotification(ctx context.Context, body []byte) error {
	var n Notification
	if err := json.Unmarshal(body, &n); err != nil || n.OrderID == "" {
		return httpx.Validation("Notifikasi tidak valid.")
	}
	id, err := s.repo.IDByProviderOrderID(ctx, s.pool, n.OrderID)
	var pid *string
	if err == nil {
		pid = &id
	} else if !database.IsNoRows(err) {
		return err
	}
	ok := s.gw.VerifySignature(n)
	if !ok {
		_ = s.repo.InsertEvent(ctx, s.pool, pid, "webhook", n.TransactionStatus.TransactionStatus, &ok, body)
		return errSignature
	}
	if pid == nil {
		// Bukan milik env ini (akun sandbox yang sama dipakai beberapa env) → abaikan.
		slog.InfoContext(ctx, "midtrans: notifikasi untuk order_id tak dikenal", "order_id", n.OrderID)
		return nil
	}
	st := n.TransactionStatus
	// Konfirmasi ke GET Status: notifikasi bisa datang tidak berurutan.
	stCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if fresh, err := s.gw.Status(stCtx, n.OrderID); err == nil {
		st = fresh
	} else {
		slog.WarnContext(ctx, "midtrans: konfirmasi status gagal, pakai isi notifikasi", "order_id", n.OrderID, "err", err)
	}
	return s.apply(ctx, id, st, "webhook", body, &ok)
}

var errSignature = httpx.NewError(http.StatusForbidden, "INVALID_SIGNATURE", "Tanda tangan notifikasi tidak valid.")

// target: status payments untuk transaction_status Midtrans ("" = tidak berubah).
func target(st TransactionStatus) (status, refund string) {
	switch st.TransactionStatus {
	case "settlement":
		return StatusPaid, ""
	case "capture":
		if st.FraudStatus == "" || st.FraudStatus == "accept" {
			return StatusPaid, ""
		}
		return "", "" // challenge: tunggu keputusan
	case "deny", "failure":
		return StatusFailed, ""
	case "expire":
		return StatusExpired, ""
	case "cancel":
		return StatusCancelled, ""
	case "refund", "partial_refund", "chargeback", "partial_chargeback":
		return "", strings.ToUpper(st.TransactionStatus)
	}
	return "", "" // pending, authorize
}

// parseAmount: "49800.00" → 49800.
func parseAmount(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int64(f + 0.5), true
}

// apply: terapkan status Midtrans secara idempotent. Saat lunas, pesanan dibuat sekali
// (dalam transaksi yang sama) dari snapshot.
func (s *Service) apply(ctx context.Context, id string, st TransactionStatus, source string, raw []byte, signatureOK *bool) error {
	var notify func()
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		notify = nil
		p, err := s.repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		pid := p.ID
		if err := s.repo.InsertEvent(ctx, tx, &pid, source, st.TransactionStatus, signatureOK, raw); err != nil {
			return err
		}
		u := update{PaymentType: st.PaymentType, ProviderTxnID: st.TransactionID, ProviderStatus: st.TransactionStatus, Checked: true}
		to, refund := target(st)

		switch {
		case to == StatusPaid && p.Status != StatusPaid:
			if amount, ok := parseAmount(st.GrossAmount); ok && amount != p.Amount {
				slog.ErrorContext(ctx, "midtrans: jumlah tidak cocok", "payment_id", id, "want", p.Amount, "got", amount)
				msg := fmt.Sprintf("Jumlah pembayaran tidak cocok (Rp %d, seharusnya Rp %d).", amount, p.Amount)
				u.Status, u.StatusMessage, u.RefundStatus = StatusFailed, &msg, "REQUIRED"
				break
			}
			paidAt := s.now()
			orderID, number, err := s.orders.CreatePaidOrder(ctx, tx, p.UserID, p.ID, p.Snapshot.Quote, p.Snapshot.Note,
				st.PaymentType, "Dibayar via "+paymentTypeLabel(st.PaymentType))
			if err != nil {
				return err
			}
			userID, paymentID, store := p.UserID, p.ID, p.StoreName
			notify = func() {
				s.notify(userID, push.Message{
					Title:    "Pembayaran berhasil 🎉",
					Body:     fmt.Sprintf("Pesanan %s diteruskan ke outlet %s.", number, store),
					ThreadID: orderID,
					Data:     map[string]string{"type": "payment_paid", "payment_id": paymentID, "order_id": orderID},
				})
			}
			msg := ""
			u.Status, u.PaidAt, u.OrderID, u.StatusMessage = StatusPaid, &paidAt, &orderID, &msg
		case to == StatusPaid:
			// sudah lunas: cukup catat
		case to != "" && p.Status == StatusPending:
			msg := st.StatusMessage
			switch to {
			case StatusExpired:
				msg = "Batas waktu pembayaran habis."
			case StatusCancelled:
				msg = "Pembayaran dibatalkan."
			case StatusFailed:
				if msg == "" {
					msg = "Pembayaran ditolak."
				}
			}
			u.Status, u.StatusMessage = to, &msg
			if to == StatusExpired {
				notify = s.expiredNotice(p)
			}
		case (to == StatusFailed || to == StatusCancelled) && p.Status == StatusPaid:
			// Reversal setelah lunas (jarang): pesanan tidak diubah otomatis, admin yang memutuskan.
			u.RefundStatus = "REVERSED"
		}
		if refund != "" {
			u.RefundStatus = refund
		}
		return s.repo.Update(ctx, tx, id, u)
	})
	if err == nil && notify != nil {
		notify() // setelah commit: tidak ada push untuk transaksi yang batal
	}
	return err
}

func (s *Service) notify(userID string, m push.Message) {
	if s.cfg.Notifier != nil {
		s.cfg.Notifier.NotifyUser(userID, m)
	}
}

func (s *Service) expiredNotice(p Payment) func() {
	return func() {
		s.notify(p.UserID, push.Message{
			Title: "Waktu pembayaran habis",
			Body:  fmt.Sprintf("Pembayaran Rp %s tidak diselesaikan, jadi pesanan belum dibuat. Silakan pesan lagi.", rupiah(p.Amount)),
			Data:  map[string]string{"type": "payment_expired", "payment_id": p.ID},
		})
	}
}

// rupiah: 54000 → "54.000".
func rupiah(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func paymentTypeLabel(t string) string {
	switch t {
	case "gopay":
		return "GoPay"
	case "shopeepay":
		return "ShopeePay"
	case "qris", "other_qris":
		return "QRIS"
	case "bank_transfer", "echannel", "permata":
		return "transfer bank (VA)"
	case "credit_card":
		return "kartu"
	case "cstore":
		return "minimarket"
	case "":
		return "Midtrans"
	}
	return t
}

func statusLabel(s string) string {
	switch s {
	case StatusPaid:
		return "lunas"
	case StatusFailed:
		return "gagal"
	case StatusExpired:
		return "kedaluwarsa"
	case StatusCancelled:
		return "dibatalkan"
	}
	return "diproses"
}

// ===== Dev =====

// DevSimulatedStatuses: transaction_status Midtrans yang bisa disimulasikan.
var DevSimulatedStatuses = []string{"settlement", "deny", "expire", "cancel"}

// DevSimulate (POST /v1/dev/payments/{id}/simulate, hanya APP_ENV=dev): terapkan status seolah
// notifikasi Midtrans datang, tanpa membayar di simulator sandbox. Lewat `apply` yang sama dengan
// webhook, jadi lunas tetap membuat pesanan & mengirim push.
func (s *Service) DevSimulate(ctx context.Context, userID, id, status, paymentType string) (Payment, error) {
	p, err := s.owned(ctx, userID, id)
	if err != nil {
		return Payment{}, err
	}
	if !contains(DevSimulatedStatuses, status) {
		return Payment{}, httpx.Validation("Status simulasi tidak valid.",
			httpx.FieldDetail{Field: "status", Message: "harus salah satu dari: " + strings.Join(DevSimulatedStatuses, ", ")})
	}
	if p.Status != StatusPending {
		return Payment{}, httpx.Unprocessable("PAYMENT_NOT_PENDING", "Pembayaran ini sudah "+statusLabel(p.Status)+".")
	}
	if paymentType == "" {
		paymentType = "qris"
	}
	st := TransactionStatus{
		OrderID: p.ProviderOrderID, TransactionID: "SIM-" + p.ID, TransactionStatus: status,
		PaymentType: paymentType, GrossAmount: fmt.Sprintf("%d.00", p.Amount), StatusMessage: "Simulasi dari debugger",
	}
	raw, _ := json.Marshal(st)
	if err := s.apply(ctx, p.ID, st, "dev-simulate", raw, nil); err != nil {
		return Payment{}, err
	}
	return s.repo.Get(ctx, s.pool, p.ID)
}
