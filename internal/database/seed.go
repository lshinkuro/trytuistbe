package database

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// DemoUser: akun demo yang sama dengan AuthMockDataSource di iOS.
type DemoUser struct {
	Email, Password, Name, Phone, Tier string
	Role                               string // "" = CUSTOMER
	StoreID                            string // wajib untuk OUTLET_ADMIN
	Points                             int
	Balance                            int64
	Vouchers                           []string
	Address                            *DemoAddress
}

type DemoAddress struct {
	Label, Recipient, Phone, Address, Notes string
	Lat, Lng                                float64
}

var DemoUsers = []DemoUser{
	// Akun web admin (trytuistadmin). Hanya untuk dev/staging; di prod promosikan
	// user lewat SQL: UPDATE users SET role = 'SUPER_ADMIN' WHERE lower(email) = '...';
	{Email: "admin@trytuist.com", Password: "admin12345", Name: "Admin TryTuist", Tier: "SILVER", Role: "SUPER_ADMIN"},
	// Admin outlet Kemang Utara: hanya pesanan, menu, stok, harga, dan promo outlet ST02.
	{Email: "kemang@trytuist.com", Password: "outlet12345", Name: "Admin Kemang", Tier: "SILVER", Role: "OUTLET_ADMIN", StoreID: "ST02"},
	{
		Email: "kholis@gmail.com", Password: "12345678", Name: "Kholis", Phone: "081234567890",
		Tier: "GOLD", Points: 1250, Balance: 50000,
		Vouchers: []string{"HEMAT10", "DISKON15K", "KOPIPAGI"},
		Address: &DemoAddress{
			Label: "Rumah", Recipient: "Kholis", Phone: "081234567890",
			Address: "Jl. Kebayoran Lama No. 12, Jakarta Selatan", Notes: "Pagar hitam",
			Lat: -6.2420, Lng: 106.7830,
		},
	},
	{
		Email: "demo@trytuist.com", Password: "password123", Name: "Demo",
		Tier: "SILVER", Vouchers: []string{"HEMAT10"},
	},
}

// Seed menjalankan file SQL di dir (urut nama) lalu membuat akun demo.
// Aman dijalankan berulang kali.
func Seed(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string, bcryptCost int) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		for _, e := range entries {
			b, err := fs.ReadFile(fsys, dir+"/"+e.Name())
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(b)); err != nil {
				return fmt.Errorf("seed %s: %w", e.Name(), err)
			}
		}
		for _, u := range DemoUsers {
			if err := seedUser(ctx, tx, u, bcryptCost); err != nil {
				return fmt.Errorf("seed user %s: %w", u.Email, err)
			}
		}
		return nil
	})
}

func seedUser(ctx context.Context, tx pgx.Tx, u DemoUser, cost int) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(u.Password), cost)
	if err != nil {
		return err
	}
	role := u.Role
	if role == "" {
		role = "CUSTOMER"
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, phone, member_tier, points, balance, role, store_id, email_verified_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, NULLIF($9, ''), now())
		ON CONFLICT (lower(email)) DO UPDATE SET password_hash = EXCLUDED.password_hash, role = EXCLUDED.role,
		    store_id = EXCLUDED.store_id, updated_at = now()
		RETURNING id::text`, u.Name, u.Email, string(hash), u.Phone, u.Tier, u.Points, u.Balance, role, u.StoreID).Scan(&id)
	if err != nil {
		return err
	}
	for _, code := range u.Vouchers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_vouchers (user_id, voucher_id)
			SELECT $1, id FROM vouchers WHERE code = $2
			ON CONFLICT DO NOTHING`, id, code); err != nil {
			return err
		}
	}
	if a := u.Address; a != nil {
		_, err := tx.Exec(ctx, `
			INSERT INTO addresses (user_id, label, recipient, phone, address, notes, lat, lng, is_default)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, TRUE
			WHERE NOT EXISTS (SELECT 1 FROM addresses WHERE user_id = $1)`,
			id, a.Label, a.Recipient, a.Phone, a.Address, a.Notes, a.Lat, a.Lng)
		if err != nil {
			return err
		}
	}
	return nil
}
