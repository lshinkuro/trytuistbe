package push

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/httpx"
)

// Notifier dipakai modul lain (order, payment). Pengiriman berjalan di background,
// jadi tidak memperlambat request / webhook.
type Notifier interface {
	NotifyUser(userID string, m Message)
}

// Nop: push nonaktif.
type Nop struct{}

func (Nop) NotifyUser(string, Message) {}

type Service struct {
	pool   *pgxpool.Pool
	sender Sender // nil = APNs belum dikonfigurasi (pesan hanya dicatat di log)
	// Wait: dipakai tes untuk menunggu pengiriman background selesai.
	inflight chan struct{}
}

func NewService(pool *pgxpool.Pool, sender Sender) *Service {
	return &Service{pool: pool, sender: sender, inflight: make(chan struct{}, 64)}
}

func (s *Service) Enabled() bool { return s.sender != nil }

var tokenRe = regexp.MustCompile(`^[0-9a-fA-F]{32,200}$`)

type DeviceInput struct {
	Token       string
	BundleID    string
	Environment string
}

// Register: simpan / pindahkan token ke user ini (satu device = satu user yang sedang login).
func (s *Service) Register(ctx context.Context, userID string, in DeviceInput) error {
	in.Token = strings.ToLower(strings.TrimSpace(in.Token))
	in.BundleID = strings.TrimSpace(in.BundleID)
	if !tokenRe.MatchString(in.Token) {
		return httpx.Validation("Data yang dikirim tidak valid.", httpx.FieldDetail{Field: "token", Message: "harus device token APNs (hex)"})
	}
	if in.BundleID == "" || len(in.BundleID) > 155 {
		return httpx.Validation("Data yang dikirim tidak valid.", httpx.FieldDetail{Field: "bundle_id", Message: "wajib diisi"})
	}
	if in.Environment != "sandbox" && in.Environment != "production" {
		return httpx.Validation("Data yang dikirim tidak valid.", httpx.FieldDetail{Field: "environment", Message: "harus sandbox atau production"})
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO push_devices (token, user_id, bundle_id, environment) VALUES ($1, $2, $3, $4)
		ON CONFLICT (token) DO UPDATE SET user_id = EXCLUDED.user_id, bundle_id = EXCLUDED.bundle_id,
			environment = EXCLUDED.environment, last_seen_at = now()`,
		in.Token, userID, in.BundleID, in.Environment)
	return err
}

// Unregister: dipanggil app saat logout / sesi berakhir. Idempotent.
func (s *Service) Unregister(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_devices WHERE token = $1`, strings.ToLower(strings.TrimSpace(token)))
	return err
}

func (s *Service) devices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT token, bundle_id, environment FROM push_devices WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Device, error) {
		var d Device
		err := row.Scan(&d.Token, &d.BundleID, &d.Environment)
		return d, err
	})
}

// NotifyUser: kirim ke semua device user di background (maks. 15 detik).
func (s *Service) NotifyUser(userID string, m Message) {
	select {
	case s.inflight <- struct{}{}:
	default:
		slog.Warn("push: antrean penuh, notifikasi dilewati", "user_id", userID, "title", m.Title)
		return
	}
	go func() {
		defer func() { <-s.inflight }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Send(ctx, userID, m); err != nil {
			slog.Warn("push: gagal", "user_id", userID, "err", err)
		}
	}()
}

// Send: kirim sinkron. Mengembalikan jumlah device yang berhasil (via log) dan error pertama.
func (s *Service) Send(ctx context.Context, userID string, m Message) error {
	devices, err := s.devices(ctx, userID)
	if err != nil {
		return err
	}
	if s.sender == nil {
		slog.Info("push (APNs belum dikonfigurasi, hanya log)", "user_id", userID, "devices", len(devices), "title", m.Title, "body", m.Body)
		return nil
	}
	var firstErr error
	for _, d := range devices {
		err := s.sender.Send(ctx, d, m)
		switch {
		case err == nil:
		case errors.Is(err, ErrUnregistered):
			slog.Info("push: token tidak berlaku, dihapus", "user_id", userID)
			_ = s.Unregister(ctx, d.Token)
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Wait: tunggu semua pengiriman background selesai (tes).
func (s *Service) Wait() {
	for i := 0; i < cap(s.inflight); i++ {
		s.inflight <- struct{}{}
	}
	for i := 0; i < cap(s.inflight); i++ {
		<-s.inflight
	}
}

// ===== Handler =====

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type deviceRequest struct {
	Token       string `json:"token" validate:"required,max=200"`
	BundleID    string `json:"bundle_id" validate:"required,max=155"`
	Environment string `json:"environment" validate:"required,oneof=sandbox production"`
}

// PUT /v1/me/devices 🔒
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req deviceRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	err := h.svc.Register(r.Context(), httpx.UserID(r.Context()), DeviceInput{
		Token: req.Token, BundleID: req.BundleID, Environment: req.Environment})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Device terdaftar", map[string]any{"push_enabled": h.svc.Enabled()})
}

type unregisterRequest struct {
	Token string `json:"token" validate:"required,max=200"`
}

// POST /v1/devices/unregister — tanpa login (dipanggil setelah token sesi dihapus).
func (h *Handler) Unregister(w http.ResponseWriter, r *http.Request) {
	var req unregisterRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if err := h.svc.Unregister(r.Context(), req.Token); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Device dihapus", nil)
}

// POST /v1/dev/push-test 🔒 (hanya APP_ENV=dev): kirim notifikasi contoh ke device sendiri.
func (h *Handler) DevTest(w http.ResponseWriter, r *http.Request) {
	err := h.svc.Send(r.Context(), httpx.UserID(r.Context()), Message{
		Title: "Tes notifikasi TryTuist 🔔", Body: "Kalau ini muncul, push sudah tersambung.",
		Data: map[string]string{"type": "test"},
	})
	if err != nil {
		httpx.FromError(w, r, httpx.NewError(http.StatusBadGateway, "PUSH_FAILED", err.Error()))
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Terkirim", map[string]any{"push_enabled": h.svc.Enabled()})
}
