package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/token"
)

// BcryptCost sesuai aturan bisnis (§7). Test boleh menurunkannya supaya cepat.
var BcryptCost = 12

// dummyHash dipakai saat email tidak terdaftar supaya waktu respons login
// sama dengan saat password salah (mencegah enumerasi email lewat timing).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.MinCost)

var errInvalidCredentials = httpx.NewError(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email atau password salah.")

// errUseGoogle: akun dibuat lewat Google (tanpa password) lalu dicoba masuk dengan password.
var errUseGoogle = httpx.NewError(http.StatusUnauthorized, "USE_GOOGLE_SIGN_IN",
	"Akun ini terdaftar dengan Google. Silakan masuk dengan Google.")

type Session struct {
	User         User
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix seconds
	// IsNewUser: true bila akun baru dibuat (register, atau Google pertama kali).
	IsNewUser bool
}

type Service struct {
	pool   *pgxpool.Pool
	repo   Repository
	tokens *token.Manager
	google *GoogleVerifier
}

type Option func(*Service)

// WithGoogle mengaktifkan POST /v1/auth/google.
func WithGoogle(v *GoogleVerifier) Option {
	return func(s *Service) { s.google = v }
}

func NewService(pool *pgxpool.Pool, tokens *token.Manager, opts ...Option) *Service {
	s := &Service{pool: pool, tokens: tokens}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type RegisterInput struct {
	Email     string
	Password  string
	Name      string
	FCMToken  string
	UserAgent string
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (Session, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), BcryptCost)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := s.repo.CreateUser(ctx, tx, name, email, string(hash), nilIfEmpty(in.FCMToken))
		if err != nil {
			if database.IsUniqueViolation(err, "users_email_key") {
				return httpx.Conflict("EMAIL_ALREADY_REGISTERED", "Email sudah terdaftar. Silakan masuk.")
			}
			return err
		}
		sess, err = s.issue(ctx, tx, u, in.UserAgent)
		sess.IsNewUser = true
		return err
	})
	return sess, err
}

type LoginInput struct {
	Email     string
	Password  string
	FCMToken  string
	UserAgent string
}

func (s *Service) Login(ctx context.Context, in LoginInput) (Session, error) {
	u, err := s.repo.UserByEmail(ctx, s.pool, strings.TrimSpace(in.Email))
	if err != nil {
		if database.IsNoRows(err) {
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
			return Session{}, errInvalidCredentials
		}
		return Session{}, err
	}
	if u.PasswordHash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
		return Session{}, errUseGoogle
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		return Session{}, errInvalidCredentials
	}
	if in.FCMToken != "" {
		if err := s.repo.UpdateFCMToken(ctx, s.pool, u.ID, in.FCMToken); err != nil {
			return Session{}, err
		}
	}
	return s.issue(ctx, s.pool, u, in.UserAgent)
}

type GoogleInput struct {
	IDToken   string
	FCMToken  string
	UserAgent string
}

var errGoogleDisabled = httpx.NewError(http.StatusServiceUnavailable, "GOOGLE_SIGN_IN_DISABLED",
	"Masuk dengan Google belum tersedia. Gunakan email dan password.")

// Google: daftar & masuk dalam satu langkah.
//  1. google_sub sudah dikenal → masuk.
//  2. email sudah terdaftar (akun email+password) → tautkan Google ke akun itu, lalu masuk.
//  3. belum ada → buat akun baru (tanpa password), IsNewUser = true.
func (s *Service) Google(ctx context.Context, in GoogleInput) (Session, error) {
	if !s.google.Enabled() {
		return Session{}, errGoogleDisabled
	}
	id, err := s.google.Verify(ctx, in.IDToken)
	if errors.Is(err, ErrInvalidGoogleToken) {
		return Session{}, httpx.NewError(http.StatusUnauthorized, "INVALID_GOOGLE_TOKEN",
			"Login Google gagal diverifikasi. Coba lagi.")
	}
	if err != nil {
		return Session{}, err // Google tidak bisa dihubungi → 500, iOS boleh coba lagi
	}
	picture := nilIfEmpty(id.Picture)
	fcm := nilIfEmpty(in.FCMToken)

	var sess Session
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		isNew := false
		u, err := s.repo.UserByGoogleSub(ctx, tx, id.Subject)
		if database.IsNoRows(err) {
			u, err = s.repo.UserByEmailForUpdate(ctx, tx, id.Email)
			switch {
			case err == nil && u.GoogleSub != nil:
				// Email sama tapi sudah tertaut ke akun Google lain.
				return httpx.Conflict("EMAIL_ALREADY_REGISTERED", "Email ini sudah tertaut ke akun Google lain.")
			case err == nil:
				u, err = s.repo.LinkGoogle(ctx, tx, u.ID, id.Subject, picture)
			case database.IsNoRows(err):
				name := id.Name
				if name == "" {
					name, _, _ = strings.Cut(id.Email, "@")
				}
				u, err = s.repo.CreateGoogleUser(ctx, tx, name, id.Email, id.Subject, picture, fcm)
				isNew = true
			}
		}
		if err != nil {
			if database.IsUniqueViolation(err) {
				return httpx.Conflict("EMAIL_ALREADY_REGISTERED", "Email sudah terdaftar. Coba masuk lagi.")
			}
			return err
		}
		if fcm != nil && !isNew {
			if err := s.repo.UpdateFCMToken(ctx, tx, u.ID, *fcm); err != nil {
				return err
			}
		}
		sess, err = s.issue(ctx, tx, u, in.UserAgent)
		sess.IsNewUser = isNew
		return err
	})
	return sess, err
}

// Refresh menukar refresh token lama dengan sesi baru (rotation). Token yang
// sudah dicabut tapi dipakai lagi dianggap bocor: semua sesi user dicabut.
func (s *Service) Refresh(ctx context.Context, raw, userAgent string) (Session, error) {
	invalid := httpx.Unauthorized("Sesi tidak valid. Silakan masuk lagi.")
	var sess Session
	var reuseUserID string
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		t, err := s.repo.RefreshTokenForUpdate(ctx, tx, token.HashRefresh(raw))
		if err != nil {
			if database.IsNoRows(err) {
				return invalid
			}
			return err
		}
		if t.RevokedAt != nil {
			reuseUserID = t.UserID
			return s.repo.RevokeAllForUser(ctx, tx, t.UserID) // commit pencabutan, lalu tolak di luar tx
		}
		if !t.ExpiresAt.After(s.now()) {
			return httpx.NewError(http.StatusUnauthorized, "TOKEN_EXPIRED", "Sesi kamu sudah berakhir. Silakan masuk lagi.")
		}
		if err := s.repo.RevokeRefreshToken(ctx, tx, t.ID); err != nil {
			return err
		}
		u, err := s.repo.UserByID(ctx, tx, t.UserID)
		if err != nil {
			return err
		}
		sess, err = s.issue(ctx, tx, u, userAgent)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if reuseUserID != "" {
		slog.WarnContext(ctx, "refresh token reuse detected; all sessions revoked", "user_id", reuseUserID)
		return Session{}, invalid
	}
	return sess, nil
}

// Logout mencabut refresh token milik user yang sedang login. Idempotent.
func (s *Service) Logout(ctx context.Context, userID, raw string) error {
	if raw == "" {
		return nil
	}
	_, err := s.repo.RevokeByHash(ctx, s.pool, userID, token.HashRefresh(raw))
	return err
}

func (s *Service) issue(ctx context.Context, db database.DBTX, u User, userAgent string) (Session, error) {
	access, exp, err := s.tokens.IssueAccess(u.ID)
	if err != nil {
		return Session{}, err
	}
	raw, hash, err := token.NewRefresh()
	if err != nil {
		return Session{}, err
	}
	if err := s.repo.InsertRefreshToken(ctx, db, u.ID, hash, s.now().Add(s.tokens.RefreshTTL()), userAgent); err != nil {
		return Session{}, err
	}
	return Session{User: u, AccessToken: access, RefreshToken: raw, ExpiresAt: exp.Unix()}, nil
}

func (s *Service) now() time.Time { return time.Now() }

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
