// Package database: koneksi pgxpool, helper transaksi, dan migrator.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect membuat pool dan memastikan database bisa dihubungi.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tidak bisa terhubung ke Postgres (%s): %w", redact(cfg.ConnConfig), err)
	}
	return pool, nil
}

func redact(c *pgx.ConnConfig) string {
	return fmt.Sprintf("%s@%s:%d/%s", c.User, c.Host, c.Port, c.Database)
}

// DBTX dipenuhi oleh *pgxpool.Pool dan pgx.Tx, jadi repository bisa dipakai
// di dalam maupun di luar transaksi.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// InTx menjalankan fn di dalam transaksi; rollback bila fn mengembalikan error.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op setelah commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation: true bila err adalah pelanggaran unique (opsional untuk constraint tertentu).
func IsUniqueViolation(err error, constraint ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	for _, c := range constraint {
		if pgErr.ConstraintName == c {
			return true
		}
	}
	return false
}

// IsCheckViolation: true bila err adalah pelanggaran CHECK (mis. stok jadi negatif).
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// IsNoRows: true bila query tidak menemukan baris.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsInvalidText: true bila Postgres menolak format input (mis. UUID tidak valid).
func IsInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
