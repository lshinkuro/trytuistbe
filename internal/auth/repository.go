// Package auth: register, login, refresh token rotation, logout.
package auth

import (
	"context"
	"time"

	"trytuist-api/internal/database"
)

type User struct {
	ID    string
	Name  string
	Email string
	// "" = akun hanya bisa masuk lewat Google.
	PasswordHash string
	AvatarURL    *string
	GoogleSub    *string
	// EmailVerified: pemilik email sudah memasukkan kode verifikasi (akun Google otomatis).
	EmailVerified bool
}

const userColumns = `id::text, name, email, COALESCE(password_hash, ''), avatar_url, google_sub, email_verified_at IS NOT NULL`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.AvatarURL, &u.GoogleSub, &u.EmailVerified)
	return u, err
}

type RefreshToken struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type Repository struct{}

func (Repository) CreateUser(ctx context.Context, db database.DBTX, name, email, hash string, fcm *string) (User, error) {
	u := User{Name: name, Email: email, PasswordHash: hash}
	err := db.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, fcm_token)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, avatar_url`, name, email, hash, fcm).Scan(&u.ID, &u.AvatarURL)
	return u, err
}

func (Repository) UserByEmail(ctx context.Context, db database.DBTX, email string) (User, error) {
	return scanUser(db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1)`, email))
}

func (Repository) UserByID(ctx context.Context, db database.DBTX, id string) (User, error) {
	return scanUser(db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// UserByEmailForUpdate: dipakai saat menautkan Google ke akun lama (cegah dua tautan bersamaan).
func (Repository) UserByEmailForUpdate(ctx context.Context, db database.DBTX, email string) (User, error) {
	return scanUser(db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1) FOR UPDATE`, email))
}

func (Repository) UserByGoogleSub(ctx context.Context, db database.DBTX, sub string) (User, error) {
	return scanUser(db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE google_sub = $1`, sub))
}

// LinkGoogle menautkan akun Google; foto profil Google dipakai bila user belum punya foto.
func (Repository) LinkGoogle(ctx context.Context, db database.DBTX, userID, sub string, picture *string) (User, error) {
	return scanUser(db.QueryRow(ctx, `
		UPDATE users SET google_sub = $2, avatar_url = COALESCE(avatar_url, $3),
		       email_verified_at = COALESCE(email_verified_at, now()), updated_at = now()
		WHERE id = $1
		RETURNING `+userColumns, userID, sub, picture))
}

func (Repository) CreateGoogleUser(ctx context.Context, db database.DBTX, name, email, sub string, picture, fcm *string) (User, error) {
	return scanUser(db.QueryRow(ctx, `
		INSERT INTO users (name, email, google_sub, avatar_url, fcm_token, email_verified_at)
		VALUES ($1, $2, $3, $4, $5, now())
		RETURNING `+userColumns, name, email, sub, picture, fcm))
}

func (Repository) UpdateFCMToken(ctx context.Context, db database.DBTX, userID, token string) error {
	_, err := db.Exec(ctx, `UPDATE users SET fcm_token = $2, updated_at = now() WHERE id = $1`, userID, token)
	return err
}

func (Repository) InsertRefreshToken(ctx context.Context, db database.DBTX, userID, hash string, expiresAt time.Time, userAgent string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at, user_agent)
		VALUES ($1, $2, $3, NULLIF($4, ''))`, userID, hash, expiresAt, truncate(userAgent, 255))
	return err
}

// RefreshTokenForUpdate mengunci baris token supaya dua refresh paralel tidak sama-sama lolos.
func (Repository) RefreshTokenForUpdate(ctx context.Context, db database.DBTX, hash string) (RefreshToken, error) {
	var t RefreshToken
	err := db.QueryRow(ctx, `
		SELECT id::text, user_id::text, expires_at, revoked_at
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, hash).
		Scan(&t.ID, &t.UserID, &t.ExpiresAt, &t.RevokedAt)
	return t, err
}

func (Repository) RevokeRefreshToken(ctx context.Context, db database.DBTX, id string) error {
	_, err := db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// RevokeByHash mencabut token milik user tertentu; mengembalikan jumlah baris yang berubah.
func (Repository) RevokeByHash(ctx context.Context, db database.DBTX, userID, hash string) (int64, error) {
	tag, err := db.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND user_id = $2 AND revoked_at IS NULL`, hash, userID)
	return tag.RowsAffected(), err
}

func (Repository) RevokeAllForUser(ctx context.Context, db database.DBTX, userID string) error {
	_, err := db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
