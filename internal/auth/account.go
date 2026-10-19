package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/mail"
)

// Lupa / ganti password & verifikasi email dengan kode 6 digit lewat email.

const (
	PurposeVerifyEmail   = "VERIFY_EMAIL"
	PurposeResetPassword = "RESET_PASSWORD"

	CodeTTL        = 15 * time.Minute
	CodeMaxAttempt = 5
	ResendCooldown = 60 * time.Second
)

var (
	errInvalidCode = httpx.Unprocessable("INVALID_CODE", "Kode salah atau sudah kedaluwarsa.")
	errCodeLocked  = httpx.Unprocessable("CODE_LOCKED", "Terlalu banyak kode salah. Minta kode baru.")
	errCooldown    = httpx.NewError(http.StatusTooManyRequests, "RATE_LIMITED", "Kode baru saja dikirim. Coba lagi dalam 1 menit.")
	errVerified    = httpx.Conflict("EMAIL_ALREADY_VERIFIED", "Email kamu sudah terverifikasi.")
	// 400 (bukan 401): klien menganggap 401 di endpoint login-only sebagai sesi habis lalu logout.
	errWrongPass = httpx.Validation("Password saat ini salah.", httpx.FieldDetail{Field: "current_password", Message: "salah"})
)

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashCode: kode di-hash bersama user & tujuan (kode reset tidak bisa dipakai untuk verifikasi).
func hashCode(userID, purpose, code string) string {
	sum := sha256.Sum256([]byte(userID + ":" + purpose + ":" + code))
	return hex.EncodeToString(sum[:])
}

// sendCode: buat kode baru (kode lama hangus) lalu kirim email. errCooldown bila baru saja dikirim.
func (s *Service) sendCode(ctx context.Context, u User, purpose string) error {
	code, err := newCode()
	if err != nil {
		return err
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Kunci user supaya dua permintaan bersamaan tidak sama-sama lolos cooldown.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u.ID); err != nil {
			return err
		}
		var recent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM email_codes
			WHERE user_id = $1 AND purpose = $2 AND created_at > $3)`, u.ID, purpose, s.now().Add(-ResendCooldown)).Scan(&recent); err != nil {
			return err
		}
		if recent {
			return errCooldown
		}
		if _, err := tx.Exec(ctx, `UPDATE email_codes SET consumed_at = now()
			WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`, u.ID, purpose); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO email_codes (user_id, purpose, code_hash, expires_at) VALUES ($1, $2, $3, $4)`,
			u.ID, purpose, hashCode(u.ID, purpose, code), s.now().Add(CodeTTL))
		return err
	})
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.mailer.Send(sendCtx, mail.CodeEmail(u.Email, u.Name, purpose, code, CodeTTL))
}

// useCode: cek kode terbaru di dalam tx. Kode salah tetap menambah `attempts` (dicatat walau ditolak),
// jadi pemanggil memakai hasil `ok` setelah tx commit, bukan error.
func (s *Service) useCode(ctx context.Context, tx pgx.Tx, userID, purpose, code string) (ok bool, err error) {
	var id int64
	var hash string
	var attempts int
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT id, code_hash, attempts, expires_at FROM email_codes
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, userID, purpose).Scan(&id, &hash, &attempts, &expires)
	if database.IsNoRows(err) {
		return false, errInvalidCode
	}
	if err != nil {
		return false, err
	}
	if s.now().After(expires) {
		return false, errInvalidCode
	}
	if attempts >= CodeMaxAttempt {
		return false, errCodeLocked
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashCode(userID, purpose, strings.TrimSpace(code)))) != 1 {
		_, err = tx.Exec(ctx, `UPDATE email_codes SET attempts = attempts + 1 WHERE id = $1`, id)
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE email_codes SET consumed_at = now() WHERE id = $1`, id)
	return err == nil, err
}

// inCodeTx: jalankan aksi bila kode benar. Kode salah → percobaan tetap tercatat, lalu errInvalidCode.
func (s *Service) inCodeTx(ctx context.Context, userID, purpose, code string, action func(tx pgx.Tx) error) error {
	wrong := false
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := s.useCode(ctx, tx, userID, purpose, code)
		if err != nil {
			return err
		}
		if !ok {
			wrong = true
			return nil // commit attempts+1
		}
		return action(tx)
	})
	if err == nil && wrong {
		return errInvalidCode
	}
	return err
}

// ForgotPassword: selalu berhasil (tidak membocorkan apakah email terdaftar). Akun Google tanpa
// password juga boleh, supaya bisa membuat password.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	u, err := s.repo.UserByEmail(ctx, s.pool, strings.TrimSpace(email))
	if database.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.sendCode(ctx, u, PurposeResetPassword); err != nil && err != errCooldown {
		slog.WarnContext(ctx, "kirim kode reset password gagal", "user_id", u.ID, "err", err)
	}
	return nil
}

// ResetPassword: kode benar → password baru, email dianggap terverifikasi (pemilik inbox),
// semua sesi lama dicabut, lalu langsung masuk.
func (s *Service) ResetPassword(ctx context.Context, email, code, password, userAgent string) (Session, error) {
	u, err := s.repo.UserByEmail(ctx, s.pool, strings.TrimSpace(email))
	if database.IsNoRows(err) {
		return Session{}, errInvalidCode
	}
	if err != nil {
		return Session{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = s.inCodeTx(ctx, u.ID, PurposeResetPassword, code, func(tx pgx.Tx) error {
		if err := setPassword(ctx, tx, u.ID, string(hash), true); err != nil {
			return err
		}
		if err := s.repo.RevokeAllForUser(ctx, tx, u.ID); err != nil {
			return err
		}
		u.EmailVerified = true
		sess, err = s.issue(ctx, tx, u, userAgent)
		return err
	})
	return sess, err
}

// ChangePassword: password lama wajib (kecuali akun Google yang belum punya password).
// Sesi lain dicabut; perangkat ini mendapat sesi baru.
func (s *Service) ChangePassword(ctx context.Context, userID, current, password, userAgent string) (Session, error) {
	u, err := s.repo.UserByID(ctx, s.pool, userID)
	if err != nil {
		return Session{}, err
	}
	if u.PasswordHash != "" && bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)) != nil {
		return Session{}, errWrongPass
	}
	if current == password {
		return Session{}, httpx.Validation("Password baru harus berbeda dari password saat ini.",
			httpx.FieldDetail{Field: "new_password", Message: "sama dengan password saat ini"})
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := setPassword(ctx, tx, u.ID, string(hash), false); err != nil {
			return err
		}
		if err := s.repo.RevokeAllForUser(ctx, tx, u.ID); err != nil {
			return err
		}
		sess, err = s.issue(ctx, tx, u, userAgent)
		return err
	})
	return sess, err
}

func setPassword(ctx context.Context, tx pgx.Tx, userID, hash string, verifyEmail bool) error {
	_, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2,
		email_verified_at = CASE WHEN $3 THEN COALESCE(email_verified_at, now()) ELSE email_verified_at END,
		updated_at = now() WHERE id = $1`, userID, hash, verifyEmail)
	return err
}

// SendVerification: kirim ulang kode verifikasi (cooldown 60 detik).
func (s *Service) SendVerification(ctx context.Context, userID string) error {
	u, err := s.repo.UserByID(ctx, s.pool, userID)
	if err != nil {
		return err
	}
	if u.EmailVerified {
		return errVerified
	}
	return s.sendCode(ctx, u, PurposeVerifyEmail)
}

func (s *Service) VerifyEmail(ctx context.Context, userID, code string) error {
	u, err := s.repo.UserByID(ctx, s.pool, userID)
	if err != nil {
		return err
	}
	if u.EmailVerified {
		return errVerified
	}
	return s.inCodeTx(ctx, userID, PurposeVerifyEmail, code, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = now(), updated_at = now() WHERE id = $1`, userID)
		return err
	})
}

// RequireVerifiedEmail: dipasang di route memesan. Akun yang belum verifikasi email → 403.
func RequireVerifiedEmail(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	errNotVerified := httpx.NewError(http.StatusForbidden, "EMAIL_NOT_VERIFIED",
		"Verifikasi email kamu dulu untuk memesan. Kode sudah dikirim ke email.")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ok bool
			err := pool.QueryRow(r.Context(), `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, httpx.UserID(r.Context())).Scan(&ok)
			if err != nil && !database.IsNoRows(err) {
				httpx.FromError(w, r, err)
				return
			}
			if !ok {
				httpx.FromError(w, r, errNotVerified)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ===== Handler =====

type forgotRequest struct {
	Email string `json:"email" validate:"required,email,max=254"`
}

type resetRequest struct {
	Email       string `json:"email" validate:"required,email,max=254"`
	Code        string `json:"code" validate:"required,len=6,numeric"`
	NewPassword string `json:"new_password" validate:"required,min=8,max=72"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"max=72"`
	NewPassword     string `json:"new_password" validate:"required,min=8,max=72"`
}

type verifyRequest struct {
	Code string `json:"code" validate:"required,len=6,numeric"`
}

// POST /v1/auth/password/forgot — selalu 200 (tidak membocorkan apakah email terdaftar).
func (h *Handler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if err := h.svc.ForgotPassword(r.Context(), req.Email); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Kalau email ini terdaftar, kode reset sudah dikirim. Cek inbox & folder spam.", nil)
}

// POST /v1/auth/password/reset — kode + password baru → langsung masuk (sesi baru).
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	sess, err := h.svc.ResetPassword(r.Context(), req.Email, req.Code, req.NewPassword, r.UserAgent())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Password diperbarui", toSessionDTO(sess))
}

// POST /v1/me/password 🔒
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	sess, err := h.svc.ChangePassword(r.Context(), httpx.UserID(r.Context()), req.CurrentPassword, req.NewPassword, r.UserAgent())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Password diperbarui. Perangkat lain sudah dikeluarkan.", toSessionDTO(sess))
}

// POST /v1/me/email/verification 🔒 — kirim ulang kode.
func (h *Handler) SendVerification(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.SendVerification(r.Context(), httpx.UserID(r.Context())); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Kode verifikasi dikirim ke email kamu.", nil)
}

// POST /v1/me/email/verify 🔒
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if err := h.svc.VerifyEmail(r.Context(), httpx.UserID(r.Context()), req.Code); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Email terverifikasi", map[string]bool{"email_verified": true})
}
