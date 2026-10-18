// Package user: profil (/v1/me), alamat, favorit, dan voucher milik user.
package user

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
)

type Profile struct {
	ID           string
	Name         string
	Email        string
	Phone        *string
	AvatarURL    *string
	MemberTier   string
	Points       int
	VoucherCount int
	Balance      int64
}

type Address struct {
	ID        string
	Label     string
	Recipient string
	Phone     string
	Address   string
	Notes     *string
	Lat       *float64
	Lng       *float64
	IsDefault bool
}

type Voucher struct {
	Code          string
	Title         string
	Description   string
	DiscountType  string
	DiscountValue int64
	MinSubtotal   int64
	MaxDiscount   *int64
	EndsAt        time.Time
}

type Repository struct{}

func (Repository) Profile(ctx context.Context, db database.DBTX, userID string) (Profile, error) {
	var p Profile
	err := db.QueryRow(ctx, `
		SELECT u.id::text, u.name, u.email, u.phone, u.avatar_url, u.member_tier, u.points, u.balance,
		       (SELECT count(*) FROM user_vouchers uv JOIN vouchers v ON v.id = uv.voucher_id
		         WHERE uv.user_id = u.id AND uv.used_at IS NULL AND v.ends_at >= now())::int
		FROM users u WHERE u.id = $1`, userID).
		Scan(&p.ID, &p.Name, &p.Email, &p.Phone, &p.AvatarURL, &p.MemberTier, &p.Points, &p.Balance, &p.VoucherCount)
	return p, err
}

type ProfilePatch struct {
	Name      *string
	Phone     *string
	AvatarURL *string
}

func (Repository) UpdateProfile(ctx context.Context, db database.DBTX, userID string, p ProfilePatch) error {
	_, err := db.Exec(ctx, `
		UPDATE users SET
		  name       = COALESCE($2, name),
		  phone      = CASE WHEN $3::text IS NULL THEN phone ELSE NULLIF($3, '') END,
		  avatar_url = CASE WHEN $4::text IS NULL THEN avatar_url ELSE NULLIF($4, '') END,
		  updated_at = now()
		WHERE id = $1`, userID, p.Name, p.Phone, p.AvatarURL)
	return err
}

const addressColumns = `id::text, label, recipient, phone, address, notes, lat, lng, is_default`

func scanAddress(row pgx.Row) (Address, error) {
	var a Address
	err := row.Scan(&a.ID, &a.Label, &a.Recipient, &a.Phone, &a.Address, &a.Notes, &a.Lat, &a.Lng, &a.IsDefault)
	return a, err
}

func (Repository) Addresses(ctx context.Context, db database.DBTX, userID string) ([]Address, error) {
	rows, err := db.Query(ctx, `SELECT `+addressColumns+` FROM addresses WHERE user_id = $1
		ORDER BY is_default DESC, created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Address, error) { return scanAddress(row) })
}

func (Repository) AddressForUpdate(ctx context.Context, db database.DBTX, id, userID string) (Address, error) {
	return scanAddress(db.QueryRow(ctx, `SELECT `+addressColumns+` FROM addresses WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
}

func (Repository) CountAddresses(ctx context.Context, db database.DBTX, userID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

func (Repository) ClearDefault(ctx context.Context, db database.DBTX, userID string) error {
	_, err := db.Exec(ctx, `UPDATE addresses SET is_default = FALSE WHERE user_id = $1 AND is_default`, userID)
	return err
}

func (Repository) InsertAddress(ctx context.Context, db database.DBTX, userID string, a Address) (Address, error) {
	return scanAddress(db.QueryRow(ctx, `
		INSERT INTO addresses (user_id, label, recipient, phone, address, notes, lat, lng, is_default)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+addressColumns,
		userID, a.Label, a.Recipient, a.Phone, a.Address, a.Notes, a.Lat, a.Lng, a.IsDefault))
}

func (Repository) SaveAddress(ctx context.Context, db database.DBTX, userID string, a Address) (Address, error) {
	return scanAddress(db.QueryRow(ctx, `
		UPDATE addresses SET label = $3, recipient = $4, phone = $5, address = $6, notes = $7, lat = $8, lng = $9, is_default = $10
		WHERE id = $1 AND user_id = $2
		RETURNING `+addressColumns,
		a.ID, userID, a.Label, a.Recipient, a.Phone, a.Address, a.Notes, a.Lat, a.Lng, a.IsDefault))
}

func (Repository) DeleteAddress(ctx context.Context, db database.DBTX, id, userID string) error {
	_, err := db.Exec(ctx, `DELETE FROM addresses WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}

// PromoteNewestDefault menjadikan alamat terbaru sebagai default bila belum ada default.
func (Repository) PromoteNewestDefault(ctx context.Context, db database.DBTX, userID string) error {
	_, err := db.Exec(ctx, `
		UPDATE addresses SET is_default = TRUE
		WHERE id = (SELECT id FROM addresses WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1)
		  AND NOT EXISTS (SELECT 1 FROM addresses WHERE user_id = $1 AND is_default)`, userID)
	return err
}

func (Repository) ProductExists(ctx context.Context, db database.DBTX, id string) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

func (Repository) AddFavorite(ctx context.Context, db database.DBTX, userID, productID string) error {
	_, err := db.Exec(ctx, `INSERT INTO favorites (user_id, product_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, productID)
	return err
}

func (Repository) RemoveFavorite(ctx context.Context, db database.DBTX, userID, productID string) error {
	_, err := db.Exec(ctx, `DELETE FROM favorites WHERE user_id = $1 AND product_id = $2`, userID, productID)
	return err
}

// Vouchers: voucher milik user yang belum dipakai & belum kedaluwarsa.
func (Repository) Vouchers(ctx context.Context, db database.DBTX, userID string) ([]Voucher, error) {
	rows, err := db.Query(ctx, `
		SELECT v.code, v.title, v.description, v.discount_type, v.discount_value, v.min_subtotal, v.max_discount, v.ends_at
		FROM user_vouchers uv JOIN vouchers v ON v.id = uv.voucher_id
		WHERE uv.user_id = $1 AND uv.used_at IS NULL AND v.ends_at >= now()
		ORDER BY v.ends_at, v.code`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Voucher, error) {
		var v Voucher
		err := row.Scan(&v.Code, &v.Title, &v.Description, &v.DiscountType, &v.DiscountValue, &v.MinSubtotal, &v.MaxDiscount, &v.EndsAt)
		return v, err
	})
}
