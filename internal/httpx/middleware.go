package httpx

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	userIDKey
)

// RequestID memakai header X-Request-ID dari client bila ada, kalau tidak membuat baru.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Logger mencatat setiap request dalam format JSON (slog).
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", ClientIP(r),
			"request_id", RequestIDFrom(r.Context()),
		}
		if uid := UserID(r.Context()); uid != "" {
			attrs = append(attrs, "user_id", uid)
		}
		slog.Log(r.Context(), level, "http", attrs...)
	})
}

// Recover menangkap panic dan mengembalikan 500 dalam format envelope.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic", "panic", rec, "stack", string(debug.Stack()),
					"request_id", RequestIDFrom(r.Context()))
				Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan pada server. Coba lagi nanti.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// APIKey memastikan header X-API-Key cocok dengan salah satu key yang diizinkan.
func APIKey(keys []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-API-Key")
			for _, k := range keys {
				if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			Error(w, http.StatusForbidden, "INVALID_API_KEY", "API key tidak valid.")
		})
	}
}

// TokenVerifier memvalidasi access token dan mengembalikan user ID.
type TokenVerifier interface {
	VerifyAccess(token string) (userID string, err error)
}

// ErrTokenExpired dikembalikan TokenVerifier bila token sudah kedaluwarsa.
var ErrTokenExpired = errors.New("token expired")

// Auth mewajibkan header Authorization: Bearer <access_token>.
func Auth(v TokenVerifier) func(http.Handler) http.Handler {
	return authMiddleware(v, true)
}

// OptionalAuth (🔒* di plan): tanpa header Authorization request tetap jalan
// sebagai tamu. Kalau header dikirim tapi token kedaluwarsa/tidak valid tetap
// dibalas 401, supaya iOS melakukan refresh token alih-alih diam-diam
// menampilkan data tamu.
func OptionalAuth(v TokenVerifier) func(http.Handler) http.Handler {
	return authMiddleware(v, false)
}

func authMiddleware(v TokenVerifier, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := strings.TrimSpace(r.Header.Get("Authorization"))
			if h == "" && !required {
				next.ServeHTTP(w, r)
				return
			}
			scheme, tok, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
				Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "Silakan masuk terlebih dahulu.")
				return
			}
			uid, err := v.VerifyAccess(strings.TrimSpace(tok))
			if err != nil {
				if errors.Is(err, ErrTokenExpired) {
					Error(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "Sesi kamu sudah berakhir. Silakan masuk lagi.")
					return
				}
				Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "Token tidak valid. Silakan masuk lagi.")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
		})
	}
}

// UserID mengambil ID user dari context (diisi middleware Auth).
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

// WithUserID dipakai di test.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// ClientIP: IP client (memperhitungkan X-Forwarded-For dari reverse proxy).
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimiter: sliding window sederhana di memori (cukup untuk satu instance).
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}, now: time.Now}
	go rl.cleanupLoop()
	return rl
}

// Allow mencatat satu percobaan untuk key dan mengembalikan false bila melewati batas.
func (rl *RateLimiter) Allow(key string) bool {
	if rl == nil || rl.limit <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	cutoff := now.Add(-rl.window)
	hits := rl.hits[key]
	i := 0
	for i < len(hits) && hits[i].Before(cutoff) {
		i++
	}
	hits = hits[i:]
	if len(hits) >= rl.limit {
		rl.hits[key] = hits
		return false
	}
	rl.hits[key] = append(hits, now)
	return true
}

// Reset menghapus hitungan untuk key (mis. setelah login berhasil).
func (rl *RateLimiter) Reset(key string) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	delete(rl.hits, key)
	rl.mu.Unlock()
}

func (rl *RateLimiter) cleanupLoop() {
	t := time.NewTicker(rl.window)
	defer t.Stop()
	for range t.C {
		rl.mu.Lock()
		cutoff := rl.now().Add(-rl.window)
		for k, hits := range rl.hits {
			if len(hits) == 0 || hits[len(hits)-1].Before(cutoff) {
				delete(rl.hits, k)
			}
		}
		rl.mu.Unlock()
	}
}

// NotFoundHandler & MethodNotAllowedHandler supaya 404/405 juga pakai envelope.
func NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	Error(w, http.StatusNotFound, "NOT_FOUND", "Endpoint tidak ditemukan.")
}

func MethodNotAllowedHandler(w http.ResponseWriter, r *http.Request) {
	Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method tidak didukung untuk endpoint ini.")
}
