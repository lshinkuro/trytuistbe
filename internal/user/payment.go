package user

// Metode pembayaran milik user (§5.21).
//
// Metode bawaan (COD, QRIS) berasal dari app_config.payment_methods dan berlaku
// untuk semua user. User hanya menyimpan akun e-wallet (tabel user_payment_methods).
// Metode utama disimpan di users.default_payment_method: 'COD' | 'QRIS' | uuid e-wallet.

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

const (
	// MaxSavedPaymentMethods: batas akun e-wallet per user.
	MaxSavedPaymentMethods = 10
	defaultBuiltinMethod   = "COD"
)

var (
	// Nomor HP Indonesia: 08xx / 628xx / +628xx, total 10–15 digit.
	ewalletNumberRe           = regexp.MustCompile(`^(?:\+?62|0)8[0-9]{7,12}$`)
	errPaymentMethodNotFound  = httpx.NotFound("Metode pembayaran tidak ditemukan.")
	builtinPaymentMethodTypes = map[string]bool{"COD": true, "QRIS": true}
)

// ===== Model & repository =====

type SavedPaymentMethod struct {
	ID            string
	Provider      string
	AccountNumber string
	AccountName   *string
}

const paymentColumns = `id::text, provider, account_number, account_name`

func scanPayment(row pgx.Row) (SavedPaymentMethod, error) {
	var m SavedPaymentMethod
	err := row.Scan(&m.ID, &m.Provider, &m.AccountNumber, &m.AccountName)
	return m, err
}

func (Repository) SavedPaymentMethods(ctx context.Context, db database.DBTX, userID string) ([]SavedPaymentMethod, error) {
	rows, err := db.Query(ctx, `SELECT `+paymentColumns+` FROM user_payment_methods WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SavedPaymentMethod, error) { return scanPayment(row) })
}

func (Repository) DefaultPaymentMethod(ctx context.Context, db database.DBTX, userID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT default_payment_method FROM users WHERE id = $1`, userID).Scan(&id)
	return id, err
}

func (Repository) SetDefaultPaymentMethod(ctx context.Context, db database.DBTX, userID, id string) error {
	_, err := db.Exec(ctx, `UPDATE users SET default_payment_method = $2, updated_at = now() WHERE id = $1`, userID, id)
	return err
}

func (Repository) CountPaymentMethods(ctx context.Context, db database.DBTX, userID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM user_payment_methods WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// InsertPaymentMethod: ok=false bila akun yang sama sudah tersimpan.
func (Repository) InsertPaymentMethod(ctx context.Context, db database.DBTX, userID string, m SavedPaymentMethod) (SavedPaymentMethod, bool, error) {
	out, err := scanPayment(db.QueryRow(ctx, `
		INSERT INTO user_payment_methods (user_id, provider, account_number, account_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, provider, account_number) DO NOTHING
		RETURNING `+paymentColumns, userID, m.Provider, m.AccountNumber, m.AccountName))
	if database.IsNoRows(err) {
		return out, false, nil
	}
	return out, err == nil, err
}

func (Repository) DeletePaymentMethod(ctx context.Context, db database.DBTX, userID, id string) (bool, error) {
	tag, err := db.Exec(ctx, `DELETE FROM user_payment_methods WHERE id = $1 AND user_id = $2`, id, userID)
	return tag.RowsAffected() > 0, err
}

func (Repository) PaymentMethodExists(ctx context.Context, db database.DBTX, userID, id string) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_payment_methods WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&ok)
	return ok, err
}

// ===== Service =====

type PaymentService struct {
	pool      *pgxpool.Pool
	repo      Repository
	appConfig *appconfig.Repository
}

func NewPaymentService(pool *pgxpool.Pool, appConfig *appconfig.Repository) *PaymentService {
	return &PaymentService{pool: pool, appConfig: appConfig}
}

// PaymentOverview: semua yang dibutuhkan screen Metode Pembayaran dalam satu response.
type PaymentOverview struct {
	DefaultID string
	Builtins  []appconfig.PaymentMethod // COD & QRIS
	EWallet   appconfig.PaymentMethod   // status global e-wallet (is_enabled)
	Saved     []SavedPaymentMethod
	Providers []appconfig.EWalletProvider
}

func (s *PaymentService) catalog(ctx context.Context) (builtins []appconfig.PaymentMethod, ewallet appconfig.PaymentMethod, providers []appconfig.EWalletProvider, err error) {
	methods, err := s.appConfig.PaymentMethods(ctx, nil)
	if err != nil {
		return nil, ewallet, nil, err
	}
	if methods == nil { // belum dikonfigurasi: semua aktif
		methods = []appconfig.PaymentMethod{
			{ID: "COD", Name: "Bayar di Tempat", Icon: "banknote", IsEnabled: true},
			{ID: "QRIS", Name: "QRIS", Icon: "qrcode", IsEnabled: true},
			{ID: "EWALLET", Name: "E-Wallet", Icon: "wallet.pass", IsEnabled: true},
		}
	}
	ewallet = appconfig.PaymentMethod{ID: "EWALLET", Name: "E-Wallet", Icon: "wallet.pass", IsEnabled: false}
	for _, m := range methods {
		switch {
		case builtinPaymentMethodTypes[m.ID]:
			builtins = append(builtins, m)
		case m.ID == "EWALLET":
			ewallet = m
		}
	}
	providers, err = s.appConfig.EWalletProviders(ctx)
	return builtins, ewallet, providers, err
}

func (s *PaymentService) Overview(ctx context.Context, userID string) (PaymentOverview, error) {
	builtins, ewallet, providers, err := s.catalog(ctx)
	if err != nil {
		return PaymentOverview{}, err
	}
	saved, err := s.repo.SavedPaymentMethods(ctx, s.pool, userID)
	if err != nil {
		return PaymentOverview{}, err
	}
	def, err := s.repo.DefaultPaymentMethod(ctx, s.pool, userID)
	if database.IsNoRows(err) {
		return PaymentOverview{}, errUserNotFound
	}
	if err != nil {
		return PaymentOverview{}, err
	}
	out := PaymentOverview{DefaultID: def, Builtins: builtins, EWallet: ewallet, Saved: saved, Providers: providers}
	// Default menunjuk metode yang sudah tidak ada / dimatikan → tampilkan COD sebagai utama.
	if !out.isUsable(def) {
		out.DefaultID = defaultBuiltinMethod
	}
	return out, nil
}

func (o PaymentOverview) isUsable(id string) bool {
	for _, b := range o.Builtins {
		if b.ID == id {
			return b.IsEnabled
		}
	}
	for _, m := range o.Saved {
		if m.ID == id {
			return o.EWallet.IsEnabled
		}
	}
	return false
}

type NewEWallet struct {
	Provider      string
	AccountNumber string
	AccountName   *string
	SetDefault    bool
}

func (s *PaymentService) AddEWallet(ctx context.Context, userID string, in NewEWallet) (PaymentOverview, string, error) {
	_, ewallet, providers, err := s.catalog(ctx)
	if err != nil {
		return PaymentOverview{}, "", err
	}
	provider := strings.ToUpper(strings.TrimSpace(in.Provider))
	number := normalizePhone(in.AccountNumber)
	var details []httpx.FieldDetail
	if !hasProvider(providers, provider) {
		details = append(details, httpx.FieldDetail{Field: "provider", Message: "penyedia e-wallet tidak dikenal"})
	}
	if !ewalletNumberRe.MatchString(number) {
		details = append(details, httpx.FieldDetail{Field: "account_number", Message: "nomor HP tidak valid, contoh 081234567890"})
	}
	if len(details) > 0 {
		return PaymentOverview{}, "", httpx.Validation("Data yang dikirim tidak valid.", details...)
	}
	if !ewallet.IsEnabled {
		return PaymentOverview{}, "", httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", "E-wallet sedang tidak tersedia.")
	}

	var created SavedPaymentMethod
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		n, err := s.repo.CountPaymentMethods(ctx, tx, userID)
		if err != nil {
			return err
		}
		if n >= MaxSavedPaymentMethods {
			return httpx.Unprocessable("PAYMENT_METHOD_LIMIT", "Maksimal 10 e-wallet tersimpan.")
		}
		var ok bool
		created, ok, err = s.repo.InsertPaymentMethod(ctx, tx, userID, SavedPaymentMethod{
			Provider: provider, AccountNumber: number, AccountName: trimPtrNil(in.AccountName),
		})
		if err != nil {
			return err
		}
		if !ok {
			return httpx.Conflict("PAYMENT_METHOD_EXISTS", "E-wallet dengan nomor ini sudah tersimpan.")
		}
		if in.SetDefault {
			return s.repo.SetDefaultPaymentMethod(ctx, tx, userID, created.ID)
		}
		return nil
	})
	if err != nil {
		return PaymentOverview{}, "", err
	}
	out, err := s.Overview(ctx, userID)
	return out, created.ID, err
}

func (s *PaymentService) SetDefault(ctx context.Context, userID, id string) (PaymentOverview, error) {
	id = strings.TrimSpace(id)
	overview, err := s.Overview(ctx, userID)
	if err != nil {
		return PaymentOverview{}, err
	}
	known := builtinPaymentMethodTypes[strings.ToUpper(id)]
	if known {
		id = strings.ToUpper(id)
	} else if uuidRe.MatchString(id) {
		ok, err := s.repo.PaymentMethodExists(ctx, s.pool, userID, id)
		if err != nil {
			return PaymentOverview{}, err
		}
		known = ok
	}
	if !known {
		return PaymentOverview{}, errPaymentMethodNotFound
	}
	if !overview.isUsable(id) {
		return PaymentOverview{}, httpx.Unprocessable("PAYMENT_METHOD_UNAVAILABLE", "Metode ini sedang tidak tersedia.")
	}
	if err := s.repo.SetDefaultPaymentMethod(ctx, s.pool, userID, id); err != nil {
		return PaymentOverview{}, err
	}
	return s.Overview(ctx, userID)
}

// Remove: hanya e-wallet tersimpan. Bila yang dihapus adalah metode utama, utama kembali ke COD.
func (s *PaymentService) Remove(ctx context.Context, userID, id string) (PaymentOverview, error) {
	if builtinPaymentMethodTypes[strings.ToUpper(id)] {
		return PaymentOverview{}, httpx.Unprocessable("PAYMENT_METHOD_BUILTIN", "Metode bawaan tidak bisa dihapus.")
	}
	if !uuidRe.MatchString(id) {
		return PaymentOverview{}, errPaymentMethodNotFound
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := s.repo.DeletePaymentMethod(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if !ok {
			return errPaymentMethodNotFound
		}
		def, err := s.repo.DefaultPaymentMethod(ctx, tx, userID)
		if err != nil {
			return err
		}
		if def == id {
			return s.repo.SetDefaultPaymentMethod(ctx, tx, userID, defaultBuiltinMethod)
		}
		return nil
	})
	if err != nil {
		return PaymentOverview{}, err
	}
	return s.Overview(ctx, userID)
}

func hasProvider(list []appconfig.EWalletProvider, id string) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

// normalizePhone: buang spasi, titik, dan strip. "+62 812-3456" → "+628123456".
func normalizePhone(s string) string {
	return strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(strings.TrimSpace(s))
}

// MaskAccount: "081234567890" → "0812••••7890". Dipakai juga oleh order (snapshot di pesanan).
func MaskAccount(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:4] + strings.Repeat("•", 4) + s[len(s)-4:]
}

// ===== DTO & handler =====

type PaymentMethodDTO struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"` // COD | QRIS | EWALLET
	Provider      *string `json:"provider"`
	Name          string  `json:"name"`
	Description   *string `json:"description"`
	Icon          string  `json:"icon"`
	AccountMasked *string `json:"account_masked"`
	AccountName   *string `json:"account_name"`
	IsEnabled     bool    `json:"is_enabled"`
	IsDefault     bool    `json:"is_default"`
	IsRemovable   bool    `json:"is_removable"`
}

type EWalletProviderDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
	Tint string `json:"tint"`
}

type PaymentOverviewDTO struct {
	DefaultID string               `json:"default_id"`
	Methods   []PaymentMethodDTO   `json:"methods"`
	Providers []EWalletProviderDTO `json:"ewallet_providers"`
	CanAdd    bool                 `json:"can_add_ewallet"`
}

func toPaymentOverviewDTO(o PaymentOverview) PaymentOverviewDTO {
	out := PaymentOverviewDTO{DefaultID: o.DefaultID, Methods: []PaymentMethodDTO{}, Providers: []EWalletProviderDTO{},
		CanAdd: o.EWallet.IsEnabled && len(o.Saved) < MaxSavedPaymentMethods && len(o.Providers) > 0}
	for _, b := range o.Builtins {
		out.Methods = append(out.Methods, PaymentMethodDTO{
			ID: b.ID, Type: b.ID, Name: b.Name, Description: nonEmpty(b.Description), Icon: b.Icon,
			IsEnabled: b.IsEnabled, IsDefault: b.ID == o.DefaultID,
		})
	}
	names := map[string]appconfig.EWalletProvider{}
	for _, p := range o.Providers {
		names[p.ID] = p
		out.Providers = append(out.Providers, EWalletProviderDTO{ID: p.ID, Name: p.Name, Icon: p.Icon, Tint: p.Tint})
	}
	for _, m := range o.Saved {
		provider, name, icon := m.Provider, m.Provider, "wallet.pass.fill"
		if p, ok := names[m.Provider]; ok {
			name, icon = p.Name, p.Icon
		}
		masked := MaskAccount(m.AccountNumber)
		out.Methods = append(out.Methods, PaymentMethodDTO{
			ID: m.ID, Type: "EWALLET", Provider: &provider, Name: name, Icon: icon,
			AccountMasked: &masked, AccountName: m.AccountName,
			IsEnabled: o.EWallet.IsEnabled, IsDefault: m.ID == o.DefaultID, IsRemovable: true,
		})
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type PaymentHandler struct {
	svc *PaymentService
}

func NewPaymentHandler(svc *PaymentService) *PaymentHandler { return &PaymentHandler{svc: svc} }

type addEWalletRequest struct {
	Provider      string  `json:"provider" validate:"required,max=30"`
	AccountNumber string  `json:"account_number" validate:"required,max=20"`
	AccountName   *string `json:"account_name" validate:"omitempty,max=100"`
	SetDefault    bool    `json:"set_default"`
}

type setDefaultPaymentRequest struct {
	ID string `json:"id" validate:"required,max=64"`
}

// GET /v1/me/payment-methods 🔒
func (h *PaymentHandler) List(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Overview(r.Context(), httpx.UserID(r.Context()))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPaymentOverviewDTO(o))
}

// POST /v1/me/payment-methods 🔒 — tautkan e-wallet. Response: overview terbaru.
func (h *PaymentHandler) Add(w http.ResponseWriter, r *http.Request) {
	var req addEWalletRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, _, err := h.svc.AddEWallet(r.Context(), httpx.UserID(r.Context()), NewEWallet{
		Provider: req.Provider, AccountNumber: req.AccountNumber, AccountName: req.AccountName, SetDefault: req.SetDefault,
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusCreated, "E-wallet ditambahkan", toPaymentOverviewDTO(o))
}

// PUT /v1/me/payment-methods/default 🔒 — body {"id":"QRIS"} atau uuid e-wallet.
func (h *PaymentHandler) SetDefault(w http.ResponseWriter, r *http.Request) {
	var req setDefaultPaymentRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	o, err := h.svc.SetDefault(r.Context(), httpx.UserID(r.Context()), req.ID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPaymentOverviewDTO(o))
}

// DELETE /v1/me/payment-methods/{id} 🔒
func (h *PaymentHandler) Remove(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Remove(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPaymentOverviewDTO(o))
}
