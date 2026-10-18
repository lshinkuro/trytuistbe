// Package db menyimpan file SQL (migrasi & seed) yang ikut ter-embed ke binary,
// jadi `go run ./cmd/migrate` dan `go run ./cmd/seed` tidak butuh CLI tambahan.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS

//go:embed seed/*.sql
var Seed embed.FS
