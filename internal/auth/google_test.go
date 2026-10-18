package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testClientID = "ios-client.apps.googleusercontent.com"

func signTestToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGoogleVerifier(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	hits := 0
	srv := jwksServer(t, key, "k1", &hits)
	v := NewGoogleVerifier([]string{testClientID}, srv.URL)
	now := time.Unix(1_800_000_000, 0)
	v.now = func() time.Time { return now }

	valid := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "aud": testClientID, "sub": "1234567890",
			"email": "Budi@Gmail.com", "email_verified": true, "name": "Budi", "picture": "https://lh3.googleusercontent.com/a/x",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
	}
	id, err := v.Verify(context.Background(), signTestToken(t, key, "k1", valid()))
	if err != nil || id.Subject != "1234567890" || id.Email != "budi@gmail.com" || id.Name != "Budi" {
		t.Fatalf("token valid ditolak: %v %+v", err, id)
	}
	// Cache: verifikasi kedua tidak mengambil JWKS lagi.
	_, _ = v.Verify(context.Background(), signTestToken(t, key, "k1", valid()))
	if hits != 1 {
		t.Fatalf("JWKS diambil %d kali, harap 1 (cache)", hits)
	}

	cases := map[string]func(c map[string]any){
		"aud lain":               func(c map[string]any) { c["aud"] = "app-lain.apps.googleusercontent.com" },
		"iss palsu":              func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		"kedaluwarsa":            func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() },
		"email belum verifikasi": func(c map[string]any) { c["email_verified"] = false },
		"email_verified string":  nil, // "true" sebagai string tetap diterima
		"tanpa email":            func(c map[string]any) { delete(c, "email") },
	}
	for name, mutate := range cases {
		c := valid()
		if mutate == nil {
			c["email_verified"] = "true"
			if _, err := v.Verify(context.Background(), signTestToken(t, key, "k1", c)); err != nil {
				t.Errorf("%s: harus diterima, dapat %v", name, err)
			}
			continue
		}
		mutate(c)
		if _, err := v.Verify(context.Background(), signTestToken(t, key, "k1", c)); err != ErrInvalidGoogleToken {
			t.Errorf("%s: harus ditolak, dapat %v", name, err)
		}
	}
	if _, err := v.Verify(context.Background(), signTestToken(t, other, "k1", valid())); err != ErrInvalidGoogleToken {
		t.Errorf("tanda tangan kunci lain harus ditolak, dapat %v", err)
	}
	if _, err := v.Verify(context.Background(), "bukan.jwt"); err != ErrInvalidGoogleToken {
		t.Errorf("format rusak harus ditolak, dapat %v", err)
	}
	if NewGoogleVerifier(nil, "").Enabled() {
		t.Error("tanpa client ID harus nonaktif")
	}
}
