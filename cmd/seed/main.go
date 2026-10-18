// Command seed mengisi data contoh (katalog, outlet, voucher, akun demo).
// Hanya untuk dev/staging.
//
//	go run ./cmd/seed
package main

import (
	"context"
	"fmt"
	"os"

	"trytuist-api/db"
	"trytuist-api/internal/config"
	"trytuist-api/internal/database"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.IsProd() {
		return fmt.Errorf("seed tidak boleh dijalankan di prod")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Seed(ctx, pool, db.Seed, "seed", 12); err != nil {
		return err
	}
	fmt.Println("✓ Seed selesai. Akun demo:")
	for _, u := range database.DemoUsers {
		fmt.Printf("   %-20s / %s\n", u.Email, u.Password)
	}
	return nil
}
