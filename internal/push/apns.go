// Package push: notifikasi push iOS lewat APNs (token-based, file .p8). API_PLAN.md §5.28.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Device: tujuan satu notifikasi.
type Device struct {
	Token       string
	BundleID    string
	Environment string // sandbox | production
}

// Message: isi notifikasi. Data ikut di payload (di luar "aps") untuk navigasi di app.
type Message struct {
	Title    string
	Body     string
	ThreadID string // pengelompokan di Notification Center, mis. id pesanan
	Data     map[string]string
}

// Sender mengirim satu notifikasi ke satu device.
type Sender interface {
	Send(ctx context.Context, d Device, m Message) error
}

// ErrUnregistered: token sudah tidak berlaku (app dihapus / token salah) → hapus dari DB.
var ErrUnregistered = errors.New("apns: device token tidak berlaku")

// APNsError: penolakan lain dari APNs (mis. InvalidProviderToken = key/team salah).
type APNsError struct {
	Status int
	Reason string
}

func (e *APNsError) Error() string { return fmt.Sprintf("apns: HTTP %d %s", e.Status, e.Reason) }

// APNs: klien HTTP/2 dengan provider token JWT (ES256), di-cache 50 menit (Apple: maks. 60).
type APNs struct {
	keyID, teamID string
	key           *ecdsa.PrivateKey
	client        *http.Client
	sandboxURL    string
	productionURL string

	mu       sync.Mutex
	jwt      string
	jwtIssue time.Time
	now      func() time.Time
}

// NewAPNs dari isi file AuthKey_XXXX.p8 (PEM PKCS#8).
func NewAPNs(keyID, teamID string, p8 []byte) (*APNs, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("apns: file .p8 bukan PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: baca .p8: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: .p8 bukan kunci EC")
	}
	if keyID == "" || teamID == "" {
		return nil, errors.New("apns: APNS_KEY_ID dan APNS_TEAM_ID wajib diisi")
	}
	return &APNs{
		keyID: keyID, teamID: teamID, key: key,
		client:        &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true}},
		sandboxURL:    "https://api.sandbox.push.apple.com",
		productionURL: "https://api.push.apple.com",
		now:           time.Now,
	}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// providerToken: JWT {alg: ES256, kid} . {iss: team, iat} ditandatangani kunci .p8.
func (a *APNs) providerToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && a.now().Sub(a.jwtIssue) < 50*time.Minute {
		return a.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.keyID})
	iat := a.now()
	claims, _ := json.Marshal(map[string]any{"iss": a.teamID, "iat": iat.Unix()})
	signing := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS ES256: r || s, masing-masing 32 byte
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	a.jwt, a.jwtIssue = signing+"."+b64(sig), iat
	return a.jwt, nil
}

func (a *APNs) invalidateToken() {
	a.mu.Lock()
	a.jwt = ""
	a.mu.Unlock()
}

// Payload APNs: {"aps": {...}, <data>}.
func payload(m Message) ([]byte, error) {
	body := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": m.Title, "body": m.Body},
			"sound": "default",
		},
	}
	if m.ThreadID != "" {
		body["aps"].(map[string]any)["thread-id"] = m.ThreadID
	}
	for k, v := range m.Data {
		if k != "aps" {
			body[k] = v
		}
	}
	return json.Marshal(body)
}

func (a *APNs) Send(ctx context.Context, d Device, m Message) error {
	token, err := a.providerToken()
	if err != nil {
		return err
	}
	b, err := payload(m)
	if err != nil {
		return err
	}
	base := a.sandboxURL
	if d.Environment == "production" {
		base = a.productionURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+d.Token, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+token)
	req.Header.Set("apns-topic", d.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Reason string `json:"reason"`
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	_ = json.Unmarshal(raw, &e)
	switch {
	case res.StatusCode == http.StatusGone,
		e.Reason == "BadDeviceToken", e.Reason == "DeviceTokenNotForTopic", e.Reason == "Unregistered":
		return ErrUnregistered
	case e.Reason == "ExpiredProviderToken" || e.Reason == "InvalidProviderToken":
		a.invalidateToken()
	}
	return &APNsError{Status: res.StatusCode, Reason: strings.TrimSpace(e.Reason)}
}

// verifyES256 dipakai tes: cek tanda tangan JWT dengan kunci publik.
func verifyES256(pub *ecdsa.PublicKey, jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
