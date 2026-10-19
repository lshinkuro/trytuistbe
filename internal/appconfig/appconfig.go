// Package appconfig: konfigurasi app yang bisa diubah tanpa update app
// (GET /v1/app-config, §5.19). Tabel app_config menyimpan satu baris per key
// dengan nilai JSON; semua key otomatis ikut di response, jadi menambah key
// baru cukup INSERT tanpa deploy backend.
package appconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// OrderRules (key "order_rules") dipakai iOS dan juga oleh server saat
// menghitung quote/checkout, jadi keduanya selalu memakai angka yang sama.
type OrderRules struct {
	MinQuantity             int   `json:"min_quantity"`
	MaxQuantity             int   `json:"max_quantity"`
	MaxNoteLength           int   `json:"max_note_length"`
	DeliveryFee             int64 `json:"delivery_fee"`
	FreeDeliveryMinSubtotal int64 `json:"free_delivery_min_subtotal"`
	// Ongkir per jarak (§7): delivery_fee berlaku sampai delivery_base_km, lalu + delivery_fee_per_km
	// tiap km berikutnya (dibulatkan ke atas). per_km 0 = flat. fee_max 0 = tanpa batas atas.
	DeliveryBaseKM   float64 `json:"delivery_base_km"`
	DeliveryFeePerKM int64   `json:"delivery_fee_per_km"`
	DeliveryFeeMax   int64   `json:"delivery_fee_max"`
	// Estimasi waktu antar = waktu siapkan pesanan + jarak / kecepatan kurir.
	PrepMinutes     int     `json:"prep_minutes"`
	CourierSpeedKMH float64 `json:"courier_speed_kmh"`
	// Kupon gratis ongkir: kuota per user per minggu, dipakai bila user mengaktifkannya saat checkout.
	FreeDeliveryCouponEnabled bool `json:"free_delivery_coupon_enabled"`
	FreeDeliveryCouponWeekly  int  `json:"free_delivery_coupon_weekly"`
}

// PaymentMethod (key "payment_methods").
type PaymentMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description,omitempty"`
	IsEnabled   bool   `json:"is_enabled"`
}

// EWalletProvider (key "ewallet_providers") — pilihan di form "Tambah e-wallet".
type EWalletProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
	Tint string `json:"tint"`
}

// HelpContacts (key "help_contacts") — kanal kontak di Pusat Bantuan.
type HelpContacts struct {
	OperatingHours string        `json:"operating_hours"`
	Channels       []HelpChannel `json:"channels"`
}

type HelpChannel struct {
	Type  string `json:"type"` // WHATSAPP | EMAIL | PHONE
	Label string `json:"label"`
	Value string `json:"value"`
	Icon  string `json:"icon"`
	URL   string `json:"url"`
}

type Repository struct {
	db       database.DBTX
	defaults OrderRules
}

// NewRepository: defaults dipakai bila key order_rules belum ada / tidak lengkap.
func NewRepository(db database.DBTX, defaults OrderRules) *Repository {
	return &Repository{db: db, defaults: defaults}
}

// All mengembalikan semua key → nilai JSON mentah.
func (r *Repository) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := r.db.Query(ctx, `SELECT key, value FROM app_config ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (r *Repository) get(ctx context.Context, db database.DBTX, key string, dst any) (bool, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT value FROM app_config WHERE key = $1`, key).Scan(&raw)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, fmt.Errorf("app_config %q bukan JSON yang valid: %w", key, err)
	}
	return true, nil
}

// OrderRules membaca aturan order; field kosong/0 diisi default.
// db boleh berupa transaksi (dipakai saat checkout).
func (r *Repository) OrderRules(ctx context.Context, db database.DBTX) (OrderRules, error) {
	if db == nil {
		db = r.db
	}
	// Mulai dari default lalu timpa dengan field yang ada di DB; field yang
	// tidak ada tetap memakai default (0 yang ditulis eksplisit tetap dipakai).
	rules := r.defaults
	if _, err := r.get(ctx, db, "order_rules", &rules); err != nil {
		return r.defaults, err
	}
	if rules.MinQuantity < 1 || rules.MaxQuantity < rules.MinQuantity || rules.MaxNoteLength < 0 ||
		rules.DeliveryFee < 0 || rules.FreeDeliveryMinSubtotal < 0 ||
		rules.DeliveryBaseKM < 0 || rules.DeliveryFeePerKM < 0 || rules.DeliveryFeeMax < 0 ||
		rules.PrepMinutes < 0 || rules.CourierSpeedKMH <= 0 || rules.FreeDeliveryCouponWeekly < 0 {
		return r.defaults, fmt.Errorf("app_config order_rules tidak valid: %+v", rules)
	}
	return rules, nil
}

// MergeOrderRules menimpa sebagian field order_rules (field lain tetap), lalu membaca ulang
// dengan validasi yang sama seperti saat quote. db sebaiknya transaksi: aturan tidak valid → rollback.
func (r *Repository) MergeOrderRules(ctx context.Context, db database.DBTX, patch map[string]any) (OrderRules, error) {
	b, err := json.Marshal(patch)
	if err != nil {
		return OrderRules{}, err
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO app_config (key, value) VALUES ('order_rules', $1::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = app_config.value || EXCLUDED.value, updated_at = now()`, b); err != nil {
		return OrderRules{}, err
	}
	return r.OrderRules(ctx, db)
}

// OrderRulesUpdatedAt: kapan order_rules terakhir diubah (nil bila belum pernah disimpan).
func (r *Repository) OrderRulesUpdatedAt(ctx context.Context) (*time.Time, error) {
	var at time.Time
	err := r.db.QueryRow(ctx, `SELECT updated_at FROM app_config WHERE key = 'order_rules'`).Scan(&at)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &at, err
}

// PaymentMethods: nil bila key belum diisi (semua metode dianggap aktif).
func (r *Repository) PaymentMethods(ctx context.Context, db database.DBTX) ([]PaymentMethod, error) {
	if db == nil {
		db = r.db
	}
	var pm []PaymentMethod
	if _, err := r.get(ctx, db, "payment_methods", &pm); err != nil {
		return nil, err
	}
	return pm, nil
}

// EWalletProviders: kosong bila key belum diisi (form tambah e-wallet tidak punya pilihan).
func (r *Repository) EWalletProviders(ctx context.Context) ([]EWalletProvider, error) {
	var out []EWalletProvider
	if _, err := r.get(ctx, r.db, "ewallet_providers", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) HelpContacts(ctx context.Context) (HelpContacts, error) {
	out := HelpContacts{Channels: []HelpChannel{}}
	if _, err := r.get(ctx, r.db, "help_contacts", &out); err != nil {
		return HelpContacts{Channels: []HelpChannel{}}, err
	}
	if out.Channels == nil {
		out.Channels = []HelpChannel{}
	}
	return out, nil
}

// ===== Handler =====

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler { return &Handler{repo: repo} }

// GET /v1/app-config — boleh di-cache (Cache-Control + ETag, 304 bila tidak berubah).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	all, err := h.repo.All(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	// order_rules selalu lengkap (default diisi bila ada field yang belum diset).
	rules, err := h.repo.OrderRules(r.Context(), nil)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	all["order_rules"], _ = json.Marshal(rules)

	body, err := json.Marshal(map[string]any{"code": 200, "message": "success", "data": all})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	w.Header().Set("Cache-Control", "max-age=300")
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" {
		for _, m := range strings.Split(match, ",") {
			if m = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m), "W/")); m == etag || m == "*" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}
