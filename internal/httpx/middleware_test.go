package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.9:5000", "1.2.3.4", "203.0.113.9"},         // langsung dari internet: header diabaikan
		{"172.18.0.5:5000", "36.80.1.2", "36.80.1.2"},          // lewat Caddy (jaringan Docker)
		{"172.18.0.5:5000", "6.6.6.6, 36.80.1.2", "36.80.1.2"}, // entri awal palsu dari klien diabaikan
		{"127.0.0.1:5000", "", "127.0.0.1"},                    // dev lokal tanpa proxy
	} {
		if got := ClientIP(req(c.remote, c.xff)); got != c.want {
			t.Errorf("remote=%s xff=%q → %s, harap %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	h := RateLimit(NewRateLimiter(2, time.Minute), func(r *http.Request) string { return r.Header.Get("X-User") })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-User", user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	call("a")
	call("a")
	if w := call("a"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("permintaan ke-3 harus 429 + Retry-After, dapat %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if call("b").Code != http.StatusNoContent {
		t.Fatal("user lain tidak ikut dibatasi")
	}
	if call("").Code != http.StatusNoContent {
		t.Fatal("key kosong tidak dihitung")
	}
}
