.DEFAULT_GOAL := help
SHELL := /bin/bash

# Database lokal (Postgres yang terinstal langsung di Mac, mis. Homebrew / Postgres.app)
DB_USER ?= trytuist
DB_PASS ?= trytuist
DB_NAME ?= trytuist
DB_TEST ?= trytuist_test
PSQL    ?= psql
TEST_DATABASE_URL ?= postgres://$(DB_USER):$(DB_PASS)@localhost:5432/$(DB_TEST)?sslmode=disable

.PHONY: help setup db-setup db-drop migrate-up migrate-down migrate-reset migrate-version seed run build test test-unit lint fmt tidy

help: ## Tampilkan daftar perintah
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

setup: ## Sekali di awal: download dependency, buat .env, buat database, migrasi & seed
	go mod tidy
	@test -f .env || (cp .env.example .env && echo "✓ .env dibuat dari .env.example")
	$(MAKE) db-setup migrate-up seed

db-setup: ## Buat role + database dev & test di Postgres lokal (aman diulang)
	@$(PSQL) -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='$(DB_USER)'" | grep -q 1 \
		|| $(PSQL) -d postgres -c "CREATE ROLE $(DB_USER) LOGIN PASSWORD '$(DB_PASS)' CREATEDB"
	@$(PSQL) -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$(DB_NAME)'" | grep -q 1 \
		|| $(PSQL) -d postgres -c "CREATE DATABASE $(DB_NAME) OWNER $(DB_USER)"
	@$(PSQL) -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$(DB_TEST)'" | grep -q 1 \
		|| $(PSQL) -d postgres -c "CREATE DATABASE $(DB_TEST) OWNER $(DB_USER)"
	@echo "✓ Database $(DB_NAME) & $(DB_TEST) siap (user: $(DB_USER))"

db-drop: ## HAPUS database dev & test
	$(PSQL) -d postgres -c "DROP DATABASE IF EXISTS $(DB_NAME)" -c "DROP DATABASE IF EXISTS $(DB_TEST)"

migrate-up: ## Terapkan semua migrasi
	go run ./cmd/migrate up

migrate-down: ## Batalkan 1 migrasi terakhir
	go run ./cmd/migrate down

migrate-reset: ## Hapus semua tabel lalu migrasi & seed ulang (dev)
	go run ./cmd/migrate down -all
	go run ./cmd/migrate up
	go run ./cmd/seed

migrate-version: ## Versi migrasi saat ini
	go run ./cmd/migrate version

seed: ## Isi data contoh + akun demo
	go run ./cmd/seed

run: ## Jalankan server di http://localhost:8080
	go run ./cmd/api

build: ## Build binary ke ./bin
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/seed ./cmd/seed

test: ## Unit + integration test (memakai database trytuist_test; isinya dihapus tiap run)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -p 1 -count=1 ./...

test-unit: ## Unit test saja (tanpa database)
	go test ./...

lint: ## go vet + cek format
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "File belum di-gofmt:"; gofmt -l .; exit 1)

fmt: ## Format kode
	gofmt -w .

tidy: ## Rapikan go.mod / go.sum
	go mod tidy
