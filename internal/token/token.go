// Package token membuat & memverifikasi access token (JWT) dan refresh token.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"trytuist-api/internal/httpx"
)

const issuer = "trytuist-api"

type Manager struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

func NewManager(secret string, accessTTL, refreshTTL time.Duration) *Manager {
	return &Manager{secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL, now: time.Now}
}

func (m *Manager) RefreshTTL() time.Duration { return m.refreshTTL }

// IssueAccess membuat JWT HS256 dengan sub = user ID.
func (m *Manager) IssueAccess(userID string) (tok string, expiresAt time.Time, err error) {
	now := m.now()
	expiresAt = now.Add(m.accessTTL)
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		Issuer:    issuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
	tok, err = jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return tok, expiresAt, err
}

// VerifyAccess memenuhi httpx.TokenVerifier.
func (m *Manager) VerifyAccess(tok string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		return m.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return "", httpx.ErrTokenExpired
		}
		return "", fmt.Errorf("invalid token: %w", err)
	}
	if claims.Subject == "" {
		return "", errors.New("invalid token: missing subject")
	}
	return claims.Subject, nil
}

// NewRefresh membuat refresh token acak "rt_<64 hex>" beserta hash SHA-256-nya.
// Yang disimpan di DB hanya hash-nya.
func NewRefresh() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = "rt_" + hex.EncodeToString(b)
	return raw, HashRefresh(raw), nil
}

func HashRefresh(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
