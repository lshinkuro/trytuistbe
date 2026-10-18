// Command migrate menjalankan migrasi database (file di db/migrations).
//
//	go run ./cmd/migrate up          # terapkan semua migrasi baru
//	go run ./cmd/migrate down        # batalkan 1 migrasi terakhir
//	go run ./cmd/migrate down -all   # batalkan semua (hapus semua tabel)
//	go run ./cmd/migrate version     # tampilkan versi saat ini
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"trytuist-api/db"
	"trytuist-api/internal/config"
	"trytuist-api/internal/database"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("pakai: migrate up | down [-all] | version")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	m := database.NewMigrator(pool, db.Migrations, "migrations")

	switch args[0] {
	case "up":
		n, err := m.Up(ctx)
		if err != nil {
			return err
		}
		v, _, _ := m.Version(ctx)
		fmt.Printf("✓ %d migrasi diterapkan, versi sekarang: %d\n", n, v)
	case "down":
		steps := 1
		if len(args) > 1 && args[1] == "-all" {
			if cfg.IsProd() {
				return fmt.Errorf("down -all diblokir di prod")
			}
			steps = 0
		}
		n, err := m.Down(ctx, steps)
		if err != nil {
			return err
		}
		v, _, _ := m.Version(ctx)
		fmt.Printf("✓ %d migrasi dibatalkan, versi sekarang: %d\n", n, v)
	case "version":
		v, dirty, err := m.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("versi: %d (dirty: %v)\n", v, dirty)
	default:
		return fmt.Errorf("perintah tidak dikenal %q (pakai: up | down [-all] | version)", args[0])
	}
	return nil
}
