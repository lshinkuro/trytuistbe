package order

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/user"
)

type Config struct {
	AppConfig *appconfig.Repository // sumber order_rules & payment_methods
	Location  *time.Location        // untuk tanggal di nomor order
}

type Service struct {
	pool *pgxpool.Pool
	repo Repository
	cfg  Config
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	return &Service{pool: pool, cfg: cfg, now: time.Now}
}

type QuoteInput struct {
	StoreID     string
	AddressID   string
	VoucherCode string
	Items       []ItemInput
}

type CreateInput struct {
	QuoteInput
	// PaymentMethod (tipe) & PaymentMethodID (COD | QRIS | uuid e-wallet) boleh kosong
	// → dipakai metode utama user (users.default_payment_method).
	PaymentMethod   string
	PaymentMethodID string
	Note            string
	IdempotencyKey  string
}

// PaymentChoice: metode pembayaran yang dipakai pesanan + snapshot akun e-wallet.
type PaymentChoice struct {
	Method        string // COD | QRIS | EWALLET
	AccountID     *string
	Provider      *string
	AccountMasked *string
}

var errPaymentMethodNotFound = httpx.NotFound("Metode pembayaran tidak ditemukan.")

// resolvePayment menentukan metode bayar pesanan:
//  1. payment_method_id diisi → COD/QRIS, atau e-wallet milik user (uuid).
//  2. hanya payment_method diisi → dipakai apa adanya (kompatibel dengan app lama).
//  3. keduanya kosong → metode utama user; bila e-wallet utama sudah dihapus → COD.
func (s *Service) resolvePayment(ctx context.Context, db database.DBTX, userID, method, methodID string) (PaymentChoice, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	id := strings.TrimSpace(methodID)
	fromDefault := false
	if id == "" && method == "" {
		def, err := s.repo.DefaultPaymentMethod(ctx, db, userID)
		if err != nil && !database.IsNoRows(err) {
			return PaymentChoice{}, err
		}
		id, fromDefault = def, true
		if id == "" {
			id = "COD"
		}
	}
	if id == "" {
		return PaymentChoice{Method: method}, nil
	}

	mismatch := func(want string) error {
		return httpx.Validation("Data yang dikirim tidak valid.", httpx.FieldDetail{
			Field: "payment_method", Message: "tidak sesuai dengan payment_method_id (" + want + ")"})
	}
	if up := strings.ToUpper(id); up == "COD" || up == "QRIS" {
		if method != "" && method != up {
			return PaymentChoice{}, mismatch(up)
		}
		return PaymentChoice{Method: up}, nil
	}
	if !uuidRe.MatchString(id) {
		if fromDefault {
			return PaymentChoice{Method: "COD"}, nil
		}
		return PaymentChoice{}, errPaymentMethodNotFound
	}
	provider, number, err := s.repo.PaymentAccount(ctx, db, id, userID)
	if database.IsNoRows(err) {
		if fromDefault {
			return PaymentChoice{Method: "COD"}, nil
		}
		return PaymentChoice{}, errPaymentMethodNotFound
	}
	if err != nil {
		return PaymentChoice{}, err
	}
	if method != "" && method != "EWALLET" {
		return PaymentChoice{}, mismatch("EWALLET")
	}
	masked := user.MaskAccount(number)
	return PaymentChoice{Method: "EWALLET", AccountID: &id, Provider: &provider, AccountMasked: &masked}, nil
}

var errOrderNotFound = httpx.NotFound("Pesanan tidak ditemukan.")

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Quote menghitung ulang harga tanpa menyimpan apa pun.
func (s *Service) Quote(ctx context.Context, userID string, in QuoteInput) (Quote, error) {
	return s.quote(ctx, s.pool, userID, in)
}

func (s *Service) quote(ctx context.Context, db database.DBTX, userID string, in QuoteInput) (Quote, error) {
	ids := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		ids = append(ids, it.ProductID)
	}
	products, err := catalog.NewRepository(db).ProductsByIDs(ctx, ids)
	if err != nil {
		return Quote{}, err
	}
	rules, err := s.cfg.AppConfig.OrderRules(ctx, db)
	if err != nil {
		return Quote{}, err
	}
	lines, subtotal, err := BuildLines(in.Items, products, Rules{
		MinQuantity: rules.MinQuantity, MaxQuantity: rules.MaxQuantity, NoteMaxChars: rules.MaxNoteLength,
	})
	if err != nil {
		return Quote{}, err
	}

	q := Quote{Lines: lines, Subtotal: subtotal}

	// Outlet
	q.StoreID = strings.TrimSpace(in.StoreID)
	if q.StoreID == "" {
		if q.StoreID, err = s.repo.DefaultStoreID(ctx, db); err != nil {
			if database.IsNoRows(err) {
				return Quote{}, httpx.Unprocessable("STORE_CLOSED", "Belum ada outlet yang tersedia.")
			}
			return Quote{}, err
		}
	}
	store, err := s.repo.StoreByID(ctx, db, q.StoreID)
	if err != nil {
		if database.IsNoRows(err) {
			return Quote{}, httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
		}
		return Quote{}, err
	}
	if !store.IsOpen {
		return Quote{}, httpx.Unprocessable("STORE_CLOSED", "Outlet "+store.Name+" sedang tutup.")
	}

	// Alamat (opsional)
	if a := strings.TrimSpace(in.AddressID); a != "" {
		ok := uuidRe.MatchString(a)
		if ok {
			if ok, err = s.repo.AddressBelongsTo(ctx, db, a, userID); err != nil {
				return Quote{}, err
			}
		}
		if !ok {
			return Quote{}, httpx.NotFound("Alamat pengiriman tidak ditemukan.")
		}
		q.AddressID = &a
	}

	q.DeliveryFee = DeliveryFee(subtotal, rules.DeliveryFee, rules.FreeDeliveryMinSubtotal)

	// Voucher (opsional)
	if code := strings.TrimSpace(in.VoucherCode); code != "" {
		v, err := s.repo.VoucherByCode(ctx, db, code)
		if err != nil {
			if database.IsNoRows(err) {
				return Quote{}, invalidVoucher("Kode voucher tidak ditemukan.")
			}
			return Quote{}, err
		}
		if err := CheckVoucher(v, subtotal, s.now()); err != nil {
			return Quote{}, err
		}
		used, err := s.repo.VoucherUsedByUser(ctx, db, userID, v.ID)
		if err != nil {
			return Quote{}, err
		}
		if used {
			return Quote{}, invalidVoucher("Voucher ini sudah pernah kamu pakai.")
		}
		if v.Quota != nil {
			n, err := s.repo.VoucherUsageCount(ctx, db, v.ID)
			if err != nil {
				return Quote{}, err
			}
			if n >= *v.Quota {
				return Quote{}, invalidVoucher("Kuota voucher sudah habis.")
			}
		}
		q.Voucher = &v
		q.Discount = Discount(v, subtotal)
	}

	q.Total = q.Subtotal + q.DeliveryFee - q.Discount
	if q.Total < 0 {
		q.Total = 0
	}
	return q, nil
}

// Create: satu transaksi DB — validasi → hitung → insert orders + order_items
// → update sold_count → tandai voucher. Idempotent lewat Idempotency-Key.
func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (Order, error) {
	var idemKey *string
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		if len(k) > 100 {
			return Order{}, httpx.Validation("Header Idempotency-Key terlalu panjang (maks. 100).")
		}
		idemKey = &k
		if id, err := s.repo.OrderIDByIdempotencyKey(ctx, s.pool, userID, k); err == nil {
			return s.Get(ctx, userID, id)
		} else if !database.IsNoRows(err) {
			return Order{}, err
		}
	}
	var note *string
	if n := strings.TrimSpace(in.Note); n != "" {
		note = &n
	}

	payment, err := s.resolvePayment(ctx, s.pool, userID, in.PaymentMethod, in.PaymentMethodID)
	if err != nil {
		return Order{}, err
	}
	if err := s.checkPaymentMethod(ctx, payment.Method); err != nil {
		return Order{}, err
	}

	var orderID string
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		quoteInput := in.QuoteInput
		// Alamat kosong → alamat utama user (bila ada). Tanpa alamat sama sekali tetap boleh (ambil di outlet).
		if strings.TrimSpace(quoteInput.AddressID) == "" {
			id, err := s.repo.DefaultAddressID(ctx, tx, userID)
			if err != nil && !database.IsNoRows(err) {
				return err
			}
			quoteInput.AddressID = id
		}
		q, err := s.quote(ctx, tx, userID, quoteInput)
		if err != nil {
			return err
		}
		number, err := s.repo.NextOrderNumber(ctx, tx, s.now().In(s.cfg.Location))
		if err != nil {
			return err
		}
		orderID, err = s.repo.InsertOrder(ctx, tx, insertOrder{
			OrderNumber: number, UserID: userID, Quote: q, PaymentMethod: payment.Method, Payment: payment,
			Note: note, IdempotencyKey: idemKey,
		})
		if err != nil {
			return err
		}
		if q.Voucher != nil {
			ok, err := s.repo.MarkVoucherUsed(ctx, tx, userID, q.Voucher.ID)
			if err != nil {
				return err
			}
			if !ok {
				return invalidVoucher("Voucher ini sudah pernah kamu pakai.")
			}
		}
		return nil
	})
	if err != nil {
		// Dua request dengan Idempotency-Key sama yang datang bersamaan.
		if idemKey != nil && database.IsUniqueViolation(err, "orders_idempotency_key") {
			id, err2 := s.repo.OrderIDByIdempotencyKey(ctx, s.pool, userID, *idemKey)
			if err2 != nil {
				return Order{}, err2
			}
			return s.Get(ctx, userID, id)
		}
		return Order{}, err
	}
	return s.Get(ctx, userID, orderID)
}

func (s *Service) List(ctx context.Context, userID string, statuses []string, page httpx.Page) ([]Summary, int, error) {
	if statuses == nil {
		statuses = []string{}
	}
	return s.repo.List(ctx, s.pool, userID, statuses, page.Limit, page.Offset())
}

func (s *Service) Get(ctx context.Context, userID, id string) (Order, error) {
	if !uuidRe.MatchString(id) {
		return Order{}, errOrderNotFound
	}
	o, err := s.repo.Get(ctx, s.pool, id, userID)
	if database.IsNoRows(err) {
		return Order{}, errOrderNotFound
	}
	return o, err
}

// Cancel: hanya untuk status PENDING.
func (s *Service) Cancel(ctx context.Context, userID, id string) (Order, error) {
	err := s.transition(ctx, userID, id, StatusCancelled, func(from string) error {
		switch from {
		case StatusPending:
			return nil
		case StatusCancelled:
			return httpx.Unprocessable("ORDER_NOT_CANCELLABLE", "Pesanan ini sudah dibatalkan.")
		default:
			return httpx.Unprocessable("ORDER_NOT_CANCELLABLE", "Pesanan tidak bisa dibatalkan karena sudah diproses.")
		}
	})
	if err != nil {
		return Order{}, err
	}
	return s.Get(ctx, userID, id)
}

// allowedTransitions mengikuti diagram status di API_PLAN.md §7.
var allowedTransitions = map[string][]string{
	StatusPending:    {StatusPaid, StatusProcessing, StatusCancelled, StatusFailed},
	StatusPaid:       {StatusProcessing},
	StatusProcessing: {StatusDelivering},
	StatusDelivering: {StatusCompleted},
}

func CanTransition(from, to string) bool {
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// AdvanceStatus dipakai endpoint khusus dev untuk mensimulasikan outlet/kurir.
func (s *Service) AdvanceStatus(ctx context.Context, userID, id, to string) (Order, error) {
	err := s.transition(ctx, userID, id, to, func(from string) error {
		if !CanTransition(from, to) {
			return httpx.Unprocessable("INVALID_STATUS_TRANSITION", fmt.Sprintf("Status tidak bisa diubah dari %s ke %s.", from, to))
		}
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.Get(ctx, userID, id)
}

func (s *Service) transition(ctx context.Context, userID, id, to string, check func(from string) error) error {
	if !uuidRe.MatchString(id) {
		return errOrderNotFound
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		from, voucherID, err := s.repo.OrderStatusForUpdate(ctx, tx, id, userID)
		if err != nil {
			if database.IsNoRows(err) {
				return errOrderNotFound
			}
			return err
		}
		if err := check(from); err != nil {
			return err
		}
		if err := s.repo.SetStatus(ctx, tx, id, to); err != nil {
			return err
		}
		if to == StatusCancelled || to == StatusFailed {
			if err := s.repo.RevertSoldCount(ctx, tx, id); err != nil {
				return err
			}
			if voucherID != nil {
				return s.repo.ReleaseVoucher(ctx, tx, userID, *voucherID)
			}
		}
		return nil
	})
}

// Rate: hanya pesanan COMPLETED, satu kali per pesanan.
func (s *Service) Rate(ctx context.Context, userID, id string, rating int, comment string) (Order, error) {
	if !uuidRe.MatchString(id) {
		return Order{}, errOrderNotFound
	}
	var c *string
	if t := strings.TrimSpace(comment); t != "" {
		c = &t
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		status, _, err := s.repo.OrderStatusForUpdate(ctx, tx, id, userID)
		if err != nil {
			if database.IsNoRows(err) {
				return errOrderNotFound
			}
			return err
		}
		if status != StatusCompleted {
			return httpx.Unprocessable("ORDER_NOT_RATABLE", "Pesanan baru bisa dinilai setelah selesai.")
		}
		if err := s.repo.InsertRating(ctx, tx, id, rating, c); err != nil {
			if database.IsUniqueViolation(err) {
				return httpx.NewError(http.StatusConflict, "ALREADY_RATED", "Pesanan ini sudah kamu nilai.")
			}
			return err
		}
		return s.repo.RecalcProductRatings(ctx, tx, id)
	})
	if err != nil {
		return Order{}, err
	}
	return s.Get(ctx, userID, id)
}

// checkPaymentMethod menolak metode yang dimatikan di app_config.payment_methods.
func (s *Service) checkPaymentMethod(ctx context.Context, id string) error {
	methods, err := s.cfg.AppConfig.PaymentMethods(ctx, nil)
	if err != nil || methods == nil {
		return err // belum dikonfigurasi = semua metode (COD/QRIS/EWALLET) boleh
	}
	for _, m := range methods {
		if m.ID == id {
			if m.IsEnabled {
				return nil
			}
			return httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", m.Name+" sedang tidak tersedia. Pilih metode lain.")
		}
	}
	return httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", "Metode pembayaran tidak tersedia.")
}
