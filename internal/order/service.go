package order

import (
	"context"
	"errors"
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
	"trytuist-api/internal/push"
	"trytuist-api/internal/user"
)

type Config struct {
	AppConfig *appconfig.Repository // sumber order_rules & payment_methods
	Location  *time.Location        // untuk tanggal di nomor order
	// OnlinePayments: Midtrans aktif (§5.27). Metode online (ONLINE/QRIS/e-wallet) wajib lewat
	// POST /v1/checkout; POST /v1/orders hanya untuk COD.
	OnlinePayments bool
	// CODDisabled: COD_ENABLED=false → "Bayar di Tempat" ditolak.
	CODDisabled bool
	// Notifier: push ke pelanggan saat status pesanan diubah outlet / admin (§5.28). nil = nonaktif.
	Notifier push.Notifier
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
	// UseFreeDelivery: user mengaktifkan kupon gratis ongkir untuk pesanan ini.
	UseFreeDelivery bool
	// skipPendingCoupons: checkout online membatalkan sesi bayar PENDING lama, jadi kuponnya tidak dihitung.
	skipPendingCoupons bool
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
	Method        string // COD | QRIS | EWALLET | ONLINE
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
	if up := strings.ToUpper(id); up == "COD" || up == "QRIS" || up == "ONLINE" {
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
	if len(in.Items) == 0 {
		return Quote{}, httpx.Unprocessable("EMPTY_CART", "Keranjang masih kosong.")
	}
	q := Quote{}
	cat := catalog.NewRepository(db)

	// Alamat (opsional): koordinatnya dipakai untuk memilih outlet & cek jangkauan antar.
	var coord *catalog.Coord
	if a := strings.TrimSpace(in.AddressID); a != "" {
		if !uuidRe.MatchString(a) {
			return Quote{}, httpx.NotFound("Alamat pengiriman tidak ditemukan.")
		}
		ok, lat, lng, err := s.repo.AddressCoord(ctx, db, a, userID)
		if err != nil {
			return Quote{}, err
		}
		if !ok {
			return Quote{}, httpx.NotFound("Alamat pengiriman tidak ditemukan.")
		}
		if lat != nil && lng != nil {
			coord = &catalog.Coord{Lat: *lat, Lng: *lng}
		}
		q.AddressID = &a
	}

	// Outlet (§5.26): pilihan user, atau yang buka & menjangkau alamat lalu terdekat.
	store, err := s.pickStore(ctx, cat, strings.TrimSpace(in.StoreID), coord)
	if err != nil {
		return Quote{}, err
	}
	if !store.IsOpenAt(s.now(), s.cfg.Location) {
		return Quote{}, httpx.Unprocessable("STORE_CLOSED", store.ClosedReason()+" Pilih outlet lain atau pesan nanti.")
	}
	if coord != nil && !store.Delivers(*coord) {
		return Quote{}, &httpx.AppError{
			Status: 422, Type: "OUT_OF_DELIVERY_AREA",
			Message: fmt.Sprintf("Alamat kamu %.1f km dari outlet %s, di luar jangkauan antar (maks. %.0f km). Pilih outlet lain.",
				store.DistanceKM(*coord), store.Name, store.RadiusKM),
			Details: []httpx.FieldDetail{{Field: "store_id", Message: "di luar jangkauan"}},
		}
	}
	q.StoreID, q.StoreName = store.ID, store.Name

	// Harga, promo, stok & menu yang dijual mengikuti outlet (§5.29).
	ids := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		ids = append(ids, it.ProductID)
	}
	products, err := cat.ProductsByIDs(ctx, ids, store.ID)
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
		var appErr *httpx.AppError
		if errors.As(err, &appErr) && appErr.Type == "PRODUCT_UNAVAILABLE" && strings.HasSuffix(appErr.Message, " sedang habis.") {
			appErr.Message = strings.TrimSuffix(appErr.Message, " sedang habis.") + " sedang habis di outlet " + store.Name + "."
		}
		return Quote{}, err
	}
	q.Lines, q.Subtotal = lines, subtotal

	if coord != nil {
		d := store.DistanceKM(*coord)
		eta := DeliveryETA(d, rules)
		q.DistanceKM, q.ETA = &d, &eta
	}
	q.DeliveryFee = DeliveryFee(subtotal, q.DistanceKM, rules)
	if err := s.applyFreeDelivery(ctx, db, userID, in, rules, &q); err != nil {
		return Quote{}, err
	}

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

// applyFreeDelivery: hitung sisa kupon minggu ini; bila diminta & ongkir > 0, ongkir ditanggung toko.
// Kupon tidak terpakai bila ongkir memang sudah 0 (mis. gratis ongkir karena subtotal).
func (s *Service) applyFreeDelivery(ctx context.Context, db database.DBTX, userID string, in QuoteInput,
	rules appconfig.OrderRules, q *Quote) error {
	since := WeekStart(s.now(), s.cfg.Location)
	st := FreeDeliveryStatus{Enabled: rules.FreeDeliveryCouponEnabled && rules.FreeDeliveryCouponWeekly > 0,
		WeeklyQuota: rules.FreeDeliveryCouponWeekly, ResetsAt: since.AddDate(0, 0, 7)}
	if st.Enabled {
		used, err := s.repo.FreeDeliveryUsed(ctx, db, userID, since, !in.skipPendingCoupons)
		if err != nil {
			return err
		}
		st.Used = used
	}
	q.FreeDelivery = &st
	if !in.UseFreeDelivery {
		return nil
	}
	if !st.Enabled {
		return httpx.Unprocessable("FREE_DELIVERY_UNAVAILABLE", "Kupon gratis ongkir sedang tidak tersedia.")
	}
	if q.DeliveryFee == 0 {
		return nil
	}
	if st.Remaining() == 0 {
		return httpx.Unprocessable("FREE_DELIVERY_QUOTA_EXCEEDED",
			fmt.Sprintf("Kupon gratis ongkir minggu ini sudah habis (%d/%d). Kuota baru mulai Senin.", st.Used, st.WeeklyQuota))
	}
	q.FreeDeliveryCoupon, q.DeliveryFeeWaived, q.DeliveryFee = true, q.DeliveryFee, 0
	return nil
}

// pickStore: outlet dari store_id, atau pilihan otomatis untuk alamat ini.
func (s *Service) pickStore(ctx context.Context, cat *catalog.Repository, storeID string, coord *catalog.Coord) (catalog.Store, error) {
	if storeID != "" {
		store, ok, err := cat.StoreByID(ctx, storeID, true)
		if err != nil {
			return catalog.Store{}, err
		}
		if !ok {
			return catalog.Store{}, httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
		}
		return store, nil
	}
	stores, err := cat.Stores(ctx)
	if err != nil {
		return catalog.Store{}, err
	}
	store, ok := catalog.PickStore(stores, coord, s.now(), s.cfg.Location)
	if !ok {
		return catalog.Store{}, httpx.Unprocessable("STORE_CLOSED", "Belum ada outlet yang tersedia.")
	}
	return store, nil
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
	if payment.Method == "ONLINE" || (s.cfg.OnlinePayments && IsOnlineMethod(payment.Method)) {
		return Order{}, httpx.Unprocessable("PAYMENT_REQUIRED", "Pesanan dengan pembayaran online dibuat setelah dibayar. Gunakan /v1/checkout.")
	}

	var orderID string
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.UseFreeDelivery {
			// Dua checkout bersamaan dari user yang sama: yang kedua menunggu, lalu menghitung kuota dari data terbaru.
			if err := s.repo.LockUser(ctx, tx, userID); err != nil {
				return err
			}
		}
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

func (s *Service) List(ctx context.Context, userID string, filter ListFilter, page httpx.Page) ([]Summary, int, error) {
	if filter.Statuses == nil {
		filter.Statuses = []string{}
	}
	return s.repo.List(ctx, s.pool, userID, filter, page.Limit, page.Offset())
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
	err := s.transition(ctx, userID, id, StatusCancelled, StatusMeta{}, func(from string) error {
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
	err := s.transition(ctx, userID, id, to, StatusMeta{}, func(from string) error {
		if !CanTransition(from, to) {
			return httpx.Unprocessable("INVALID_STATUS_TRANSITION", fmt.Sprintf("Status tidak bisa diubah dari %s ke %s.", from, to))
		}
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	o, err := s.Get(ctx, userID, id)
	if err == nil {
		s.notifyStatus(o, "")
	}
	return o, err
}

// notifyStatus: push "pesanan disiapkan / diantar / selesai / gagal" ke pemilik pesanan.
func (s *Service) notifyStatus(o Order, note string) {
	if s.cfg.Notifier == nil {
		return
	}
	title, body := "", ""
	switch o.Status {
	case StatusProcessing:
		title, body = "Pesanan sedang disiapkan 👨‍🍳", fmt.Sprintf("%s sedang disiapkan di outlet %s.", o.OrderNumber, o.Store.Name)
	case StatusDelivering:
		title, body = "Pesanan dalam perjalanan 🛵", fmt.Sprintf("Kurir sedang mengantar %s ke alamatmu.", o.OrderNumber)
	case StatusCompleted:
		title, body = "Pesanan selesai ✅", fmt.Sprintf("Selamat menikmati! Beri nilai pesanan %s, yuk.", o.OrderNumber)
	case StatusFailed:
		title, body = "Pesanan gagal", o.OrderNumber
	case StatusCancelled:
		title, body = "Pesanan dibatalkan", o.OrderNumber
	default:
		return
	}
	if note != "" {
		body += ": " + note
	} else if o.Status == StatusFailed || o.Status == StatusCancelled {
		body += "."
	}
	s.cfg.Notifier.NotifyUser(o.UserID, push.Message{
		Title: title, Body: body, ThreadID: o.ID,
		Data: map[string]string{"type": "order_status", "order_id": o.ID, "status": o.Status},
	})
}

// ===== Admin (§5.25) =====

// IsFinal: status akhir yang tidak bisa diubah lagi.
func IsFinal(status string) bool {
	return status == StatusCompleted || status == StatusCancelled || status == StatusFailed
}

// statusRank: urutan alur normal pesanan.
var statusRank = map[string]int{StatusPending: 0, StatusPaid: 1, StatusProcessing: 2, StatusDelivering: 3, StatusCompleted: 4}

// CanAdminTransition: admin boleh memajukan status (boleh melompat, mis. PENDING → COMPLETED
// = "sukses"), atau menandai GAGAL / BATAL selama pesanan belum selesai. Status akhir terkunci.
func CanAdminTransition(from, to string) bool {
	if IsFinal(from) || from == to {
		return false
	}
	if to == StatusFailed || to == StatusCancelled {
		return true
	}
	fr, ok1 := statusRank[from]
	tr, ok2 := statusRank[to]
	return ok1 && ok2 && tr > fr
}

// AdminNextStatuses: pilihan status di web admin untuk pesanan berstatus from.
func AdminNextStatuses(from string) []string {
	out := []string{}
	for _, to := range AllStatuses {
		if CanAdminTransition(from, to) {
			out = append(out, to)
		}
	}
	return out
}

// AdminSetStatus mengubah status pesanan milik user mana pun. GAGAL / BATAL wajib beri alasan;
// sold_count & voucher dikembalikan seperti pembatalan biasa.
func (s *Service) AdminSetStatus(ctx context.Context, id, to string, meta StatusMeta) (Order, string, error) {
	if !uuidRe.MatchString(id) {
		return Order{}, "", errOrderNotFound
	}
	meta.Note = strings.TrimSpace(meta.Note)
	if (to == StatusFailed || to == StatusCancelled) && meta.Note == "" {
		return Order{}, "", httpx.Validation("Alasan wajib diisi untuk status gagal / batal.",
			httpx.FieldDetail{Field: "note", Message: "wajib diisi"})
	}
	userID, err := s.repo.OrderOwner(ctx, s.pool, id)
	if database.IsNoRows(err) {
		return Order{}, "", errOrderNotFound
	}
	if err != nil {
		return Order{}, "", err
	}
	var previous string
	err = s.transition(ctx, userID, id, to, meta, func(from string) error {
		previous = from
		if !CanAdminTransition(from, to) {
			if IsFinal(from) {
				return httpx.Unprocessable("INVALID_STATUS_TRANSITION", fmt.Sprintf("Pesanan sudah %s dan tidak bisa diubah lagi.", from))
			}
			return httpx.Unprocessable("INVALID_STATUS_TRANSITION", fmt.Sprintf("Status tidak bisa diubah dari %s ke %s.", from, to))
		}
		return nil
	})
	if err != nil {
		return Order{}, "", err
	}
	o, err := s.Get(ctx, userID, id)
	if err == nil {
		s.notifyStatus(o, meta.Note)
	}
	return o, previous, err
}

// AdminGet: detail pesanan siapa pun.
func (s *Service) AdminGet(ctx context.Context, id string) (Order, error) {
	if !uuidRe.MatchString(id) {
		return Order{}, errOrderNotFound
	}
	userID, err := s.repo.OrderOwner(ctx, s.pool, id)
	if database.IsNoRows(err) {
		return Order{}, errOrderNotFound
	}
	if err != nil {
		return Order{}, err
	}
	return s.Get(ctx, userID, id)
}

func (s *Service) transition(ctx context.Context, userID, id, to string, meta StatusMeta, check func(from string) error) error {
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
		if err := s.repo.SetStatus(ctx, tx, id, to, meta); err != nil {
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

// checkPaymentMethod menolak metode yang dimatikan di app_config.payment_methods
// (atau lewat env: COD_ENABLED=false, ONLINE tanpa MIDTRANS_SERVER_KEY).
func (s *Service) checkPaymentMethod(ctx context.Context, id string) error {
	if id == "COD" && s.cfg.CODDisabled {
		return httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", "Bayar di Tempat sedang tidak tersedia. Pilih metode lain.")
	}
	if id == "ONLINE" && !s.cfg.OnlinePayments {
		return httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", "Pembayaran online sedang tidak tersedia. Pilih metode lain.")
	}
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
