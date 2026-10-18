package database

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrator menjalankan file NNNN_nama.up.sql / .down.sql.
//
// Penamaan file dan tabel schema_migrations(version, dirty) sengaja dibuat
// kompatibel dengan golang-migrate, jadi kalau nanti mau pakai CLI `migrate`
// di pipeline CI, state-nya tetap terbaca.
type Migrator struct {
	pool *pgxpool.Pool
	fsys fs.FS
	dir  string
}

type migration struct {
	version int64
	name    string
	up      string
	down    string
}

var fileRe = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

func NewMigrator(pool *pgxpool.Pool, fsys fs.FS, dir string) *Migrator {
	return &Migrator{pool: pool, fsys: fsys, dir: dir}
}

func (m *Migrator) load() ([]migration, error) {
	entries, err := fs.ReadDir(m.fsys, m.dir)
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]*migration{}
	for _, e := range entries {
		match := fileRe.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		v, _ := strconv.ParseInt(match[1], 10, 64)
		b, err := fs.ReadFile(m.fsys, m.dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		mg := byVersion[v]
		if mg == nil {
			mg = &migration{version: v, name: match[2]}
			byVersion[v] = mg
		}
		if match[3] == "up" {
			mg.up = string(b)
		} else {
			mg.down = string(b)
		}
	}
	out := make([]migration, 0, len(byVersion))
	for _, mg := range byVersion {
		out = append(out, *mg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT NOT NULL PRIMARY KEY, dirty BOOLEAN NOT NULL)`)
	return err
}

// Version mengembalikan versi saat ini (0 = belum ada migrasi).
func (m *Migrator) Version(ctx context.Context) (version int64, dirty bool, err error) {
	if err = m.ensureTable(ctx); err != nil {
		return 0, false, err
	}
	err = m.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if IsNoRows(err) {
		return 0, false, nil
	}
	return version, dirty, err
}

func setVersion(ctx context.Context, tx pgx.Tx, v int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations`); err != nil {
		return err
	}
	if v == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)`, v)
	return err
}

// Up menjalankan semua migrasi yang belum diterapkan. Setiap file berjalan di transaksi sendiri.
func (m *Migrator) Up(ctx context.Context) (applied int, err error) {
	cur, dirty, err := m.Version(ctx)
	if err != nil {
		return 0, err
	}
	if dirty {
		return 0, fmt.Errorf("database dalam status dirty di versi %d; perbaiki manual dulu", cur)
	}
	all, err := m.load()
	if err != nil {
		return 0, err
	}
	for _, mg := range all {
		if mg.version <= cur {
			continue
		}
		if mg.up == "" {
			return applied, fmt.Errorf("migrasi %d tidak punya file .up.sql", mg.version)
		}
		err := InTx(ctx, m.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, mg.up); err != nil {
				return fmt.Errorf("migrasi %04d_%s gagal: %w", mg.version, mg.name, err)
			}
			return setVersion(ctx, tx, mg.version)
		})
		if err != nil {
			return applied, err
		}
		slog.Info("migrate up", "version", mg.version, "name", mg.name)
		applied++
	}
	return applied, nil
}

// Down membatalkan `steps` migrasi terakhir (steps <= 0 = semua).
func (m *Migrator) Down(ctx context.Context, steps int) (reverted int, err error) {
	cur, dirty, err := m.Version(ctx)
	if err != nil {
		return 0, err
	}
	if dirty {
		return 0, fmt.Errorf("database dalam status dirty di versi %d; perbaiki manual dulu", cur)
	}
	all, err := m.load()
	if err != nil {
		return 0, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		mg := all[i]
		if mg.version > cur {
			continue
		}
		if steps > 0 && reverted >= steps {
			break
		}
		prev := int64(0)
		if i > 0 {
			prev = all[i-1].version
		}
		err := InTx(ctx, m.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, mg.down); err != nil {
				return fmt.Errorf("rollback %04d_%s gagal: %w", mg.version, mg.name, err)
			}
			return setVersion(ctx, tx, prev)
		})
		if err != nil {
			return reverted, err
		}
		slog.Info("migrate down", "version", mg.version, "name", mg.name)
		reverted++
	}
	return reverted, nil
}
