// Package config membaca konfigurasi dari environment (dan file .env bila ada).
package config

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env                     string // dev | staging | prod
	Port                    string
	DatabaseURL             string
	APIKeys                 []string
	JWTSecret               string
	JWTAccessTTL            time.Duration
	JWTRefreshTTL           time.Duration
	LogLevel                slog.Level
	Timezone                *time.Location
	DeliveryFee             int64
	FreeDeliveryMinSubtotal int64 // 0 = nonaktif
	LoginRateLimit          int   // percobaan per menit per IP + email
	// Google Sign-In: OAuth client ID iOS (boleh lebih dari satu: Dev/Staging/Prod). Kosong = nonaktif.
	GoogleClientIDs []string
	GoogleJWKSURL   string
	// Pembayaran online Midtrans (§5.27). Server key kosong = Bayar Online nonaktif.
	MidtransServerKey       string
	MidtransIsProduction    bool
	MidtransEnabledPayments []string
	PublicBaseURL           string // untuk finish URL Snap
	PaymentExpiry           time.Duration
	CODDisabled             bool // COD_ENABLED=false
	// Push iOS lewat APNs (§5.28). Kosong = notifikasi hanya dicatat di log.
	APNsKeyID   string
	APNsTeamID  string
	APNsKeyFile string // path file AuthKey_XXXX.p8
}

func (c Config) IsDev() bool  { return c.Env == "dev" }
func (c Config) IsProd() bool { return c.Env == "prod" }

// Load membaca .env (tanpa menimpa env yang sudah di-set) lalu memvalidasi nilai.
func Load() (Config, error) {
	loadDotEnv(".env")

	cfg := Config{
		Env:         get("APP_ENV", "dev"),
		Port:        get("PORT", "8080"),
		DatabaseURL: get("DATABASE_URL", "postgres://trytuist:trytuist@localhost:5432/trytuist?sslmode=disable"),
		JWTSecret:   get("JWT_SECRET", ""),
	}
	switch cfg.Env {
	case "dev", "staging", "prod":
	default:
		return cfg, fmt.Errorf("APP_ENV harus dev | staging | prod, dapat %q", cfg.Env)
	}

	for _, k := range strings.Split(get("API_KEYS", "dev-local-api-key"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			cfg.APIKeys = append(cfg.APIKeys, k)
		}
	}
	if len(cfg.APIKeys) == 0 {
		return cfg, fmt.Errorf("API_KEYS wajib diisi")
	}

	if cfg.JWTSecret == "" {
		if !cfg.IsDev() {
			return cfg, fmt.Errorf("JWT_SECRET wajib diisi di %s", cfg.Env)
		}
		cfg.JWTSecret = "dev-only-insecure-secret-change-me"
	}
	if !cfg.IsDev() && len(cfg.JWTSecret) < 32 {
		return cfg, fmt.Errorf("JWT_SECRET minimal 32 karakter di %s", cfg.Env)
	}

	var err error
	if cfg.JWTAccessTTL, err = time.ParseDuration(get("JWT_ACCESS_TTL", "15m")); err != nil {
		return cfg, fmt.Errorf("JWT_ACCESS_TTL: %w", err)
	}
	if cfg.JWTRefreshTTL, err = time.ParseDuration(get("JWT_REFRESH_TTL", "720h")); err != nil {
		return cfg, fmt.Errorf("JWT_REFRESH_TTL: %w", err)
	}
	if err = cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", defaultLogLevel(cfg.Env)))); err != nil {
		return cfg, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if cfg.Timezone, err = time.LoadLocation(get("TIMEZONE", "Asia/Jakarta")); err != nil {
		return cfg, fmt.Errorf("TIMEZONE: %w", err)
	}
	if cfg.DeliveryFee, err = getInt("DELIVERY_FEE", 10000); err != nil {
		return cfg, err
	}
	if cfg.FreeDeliveryMinSubtotal, err = getInt("FREE_DELIVERY_MIN_SUBTOTAL", 0); err != nil {
		return cfg, err
	}
	rl, err := getInt("LOGIN_RATE_LIMIT", 5)
	if err != nil {
		return cfg, err
	}
	cfg.LoginRateLimit = int(rl)
	for _, id := range strings.Split(get("GOOGLE_CLIENT_IDS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.GoogleClientIDs = append(cfg.GoogleClientIDs, id)
		}
	}
	cfg.GoogleJWKSURL = get("GOOGLE_JWKS_URL", "https://www.googleapis.com/oauth2/v3/certs")

	cfg.MidtransServerKey = get("MIDTRANS_SERVER_KEY", "")
	cfg.MidtransIsProduction = strings.EqualFold(get("MIDTRANS_IS_PRODUCTION", "false"), "true")
	if cfg.MidtransIsProduction && strings.HasPrefix(cfg.MidtransServerKey, "SB-") {
		return cfg, fmt.Errorf("MIDTRANS_IS_PRODUCTION=true tapi MIDTRANS_SERVER_KEY adalah key sandbox (SB-…)")
	}
	for _, p := range strings.Split(get("MIDTRANS_ENABLED_PAYMENTS", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.MidtransEnabledPayments = append(cfg.MidtransEnabledPayments, p)
		}
	}
	cfg.PublicBaseURL = strings.TrimRight(get("PUBLIC_BASE_URL", "http://localhost:"+cfg.Port), "/")
	if cfg.PaymentExpiry, err = time.ParseDuration(get("PAYMENT_EXPIRY", "15m")); err != nil || cfg.PaymentExpiry < time.Minute {
		return cfg, fmt.Errorf("PAYMENT_EXPIRY harus durasi ≥ 1m (mis. 15m)")
	}
	cfg.CODDisabled = strings.EqualFold(get("COD_ENABLED", "true"), "false")

	cfg.APNsKeyID = get("APNS_KEY_ID", "")
	cfg.APNsTeamID = get("APNS_TEAM_ID", "")
	cfg.APNsKeyFile = get("APNS_KEY_FILE", "")
	if cfg.APNsKeyFile != "" && (cfg.APNsKeyID == "" || cfg.APNsTeamID == "") {
		return cfg, fmt.Errorf("APNS_KEY_FILE diisi tapi APNS_KEY_ID / APNS_TEAM_ID kosong")
	}
	return cfg, nil
}

func defaultLogLevel(env string) string {
	if env == "dev" {
		return "debug"
	}
	return "info"
}

func get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int64) (int64, error) {
	v := get(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s harus angka: %w", key, err)
	}
	return n, nil
}

// loadDotEnv: parser .env sederhana (KEY=VALUE, komentar `#`, tanda kutip opsional).
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
