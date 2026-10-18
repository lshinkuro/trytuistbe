package auth

// Verifikasi Google ID token (JWT RS256) dari Google Sign-In iOS (§5.3b).
//
// Hanya memakai standard library: kunci publik Google (JWKS) diambil dari
// GOOGLE_JWKS_URL dan di-cache sesuai Cache-Control. Yang dicek:
//   - tanda tangan RS256 dengan kunci `kid` yang cocok
//   - iss = accounts.google.com, aud = salah satu GOOGLE_CLIENT_IDS
//   - exp belum lewat, email ada & email_verified = true

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGoogleJWKSURL: kunci publik resmi Google untuk ID token.
const DefaultGoogleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

// GoogleIdentity: data user dari ID token yang sudah terverifikasi.
type GoogleIdentity struct {
	Subject string // id akun Google (stabil, unik)
	Email   string
	Name    string
	Picture string
}

// ErrInvalidGoogleToken: token palsu, kedaluwarsa, atau bukan untuk app ini.
var ErrInvalidGoogleToken = errors.New("google id token tidak valid")

type GoogleVerifier struct {
	clientIDs map[string]bool
	jwksURL   string
	client    *http.Client
	now       func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
	lastFetch time.Time
}

// NewGoogleVerifier: clientIDs kosong = Google Sign-In nonaktif (Enabled() false).
func NewGoogleVerifier(clientIDs []string, jwksURL string) *GoogleVerifier {
	ids := map[string]bool{}
	for _, id := range clientIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = true
		}
	}
	if jwksURL == "" {
		jwksURL = DefaultGoogleJWKSURL
	}
	return &GoogleVerifier{
		clientIDs: ids, jwksURL: jwksURL,
		client: &http.Client{Timeout: 5 * time.Second}, now: time.Now,
	}
}

func (v *GoogleVerifier) Enabled() bool { return v != nil && len(v.clientIDs) > 0 }

// clockSkew: toleransi beda jam server dan Google.
const clockSkew = time.Minute

func (v *GoogleVerifier) Verify(ctx context.Context, raw string) (GoogleIdentity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return GoogleIdentity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}

	var claims struct {
		Iss           string          `json:"iss"`
		Aud           string          `json:"aud"`
		Sub           string          `json:"sub"`
		Exp           json.Number     `json:"exp"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"` // Google kadang mengirim bool, kadang string
		Name          string          `json:"name"`
		Picture       string          `json:"picture"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}
	exp, err := claims.Exp.Int64()
	now := v.now()
	switch {
	case claims.Iss != "accounts.google.com" && claims.Iss != "https://accounts.google.com",
		!v.clientIDs[claims.Aud],
		err != nil || now.After(time.Unix(exp, 0).Add(clockSkew)),
		claims.Sub == "", claims.Email == "",
		!isTrue(claims.EmailVerified):
		return GoogleIdentity{}, ErrInvalidGoogleToken
	}
	return GoogleIdentity{
		Subject: claims.Sub,
		Email:   strings.ToLower(strings.TrimSpace(claims.Email)),
		Name:    strings.TrimSpace(claims.Name),
		Picture: claims.Picture,
	}, nil
}

// key mengambil kunci publik dari cache; JWKS diambil ulang bila cache habis
// atau kid belum dikenal (Google merotasi kunci), maksimal sekali per menit.
func (v *GoogleVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && now.Before(v.expiresAt) {
		return k, nil
	}
	if now.Sub(v.lastFetch) < time.Minute && v.keys != nil {
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, ErrInvalidGoogleToken
	}
	v.lastFetch = now
	keys, ttl, err := v.fetch(ctx)
	if err != nil {
		if k, ok := v.keys[kid]; ok { // Google tidak bisa dihubungi: pakai cache lama
			return k, nil
		}
		return nil, err
	}
	v.keys, v.expiresAt = keys, now.Add(ttl)
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, ErrInvalidGoogleToken
}

var maxAgeRe = regexp.MustCompile(`max-age=(\d+)`)

func (v *GoogleVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, 0, err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ambil JWKS Google: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("ambil JWKS Google: status %d", res.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, 0, fmt.Errorf("JWKS Google bukan JSON valid: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range body.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	ttl := time.Hour
	if m := maxAgeRe.FindStringSubmatch(res.Header.Get("Cache-Control")); m != nil {
		if s, err := strconv.Atoi(m[1]); err == nil && s > 0 {
			ttl = time.Duration(s) * time.Second
		}
	}
	return keys, ttl, nil
}

func decodeSegment(seg string, dst any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	return dec.Decode(dst)
}

func isTrue(raw json.RawMessage) bool {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return s == "true"
}
