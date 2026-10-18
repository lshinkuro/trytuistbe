# TryTuist API (Go + PostgreSQL)

Backend untuk aplikasi iOS **TryTuist**. Kontraknya ada di [`API_PLAN.md`](API_PLAN.md); semua nama field JSON sama dengan DTO di iOS.

Semua endpoint di plan (#1–#27, fase 1, 1b, 2, 3) sudah jalan dan dites.

## Jalankan di lokal (Postgres terinstal langsung di Mac)

Butuh **Go 1.23+** dan **PostgreSQL 13+** yang sedang berjalan (`brew services start postgresql@16` atau Postgres.app).

```bash
cd ~/Documents/iOS/TryTuistBe
make setup      # go mod tidy → buat .env → buat role+DB "trytuist" & "trytuist_test" → migrasi → seed
make run        # http://localhost:8080
```

Cek:

```bash
curl localhost:8080/healthz
# {"status":"ok","env":"dev","db":"ok"}

curl -s localhost:8080/v1/auth/login \
  -H 'X-API-Key: dev-local-api-key' -H 'Content-Type: application/json' \
  -d '{"email":"kholis@gmail.com","password":"12345678"}'
```

Akun demo (sama dengan `AuthMockDataSource`): `kholis@gmail.com / 12345678` dan `demo@trytuist.com / password123`.

> `make db-setup` memakai `psql -d postgres` dengan user Mac kamu (default Homebrew). Kalau Postgres-mu butuh user lain: `make db-setup PSQL="psql -U postgres"`. Kalau lebih suka Docker: `docker compose up -d`, lalu lewati `db-setup` (database `trytuist` sudah dibuat oleh container).

### Perintah lain

| Perintah | Fungsi |
|---|---|
| `make test` | Unit + integration test terhadap DB `trytuist_test` (isinya di-reset tiap run) |
| `make migrate-up` / `migrate-down` | Terapkan / batalkan 1 migrasi |
| `make migrate-reset` | Hapus semua tabel, migrasi & seed ulang |
| `make seed` | Isi ulang data contoh (aman diulang) |
| `make lint` | `go vet` + cek `gofmt` |
| `make build` | Binary ke `./bin` |

## Struktur

```
cmd/
  api/        server HTTP (graceful shutdown)
  migrate/    up | down [-all] | version
  seed/       data contoh + akun demo (bcrypt)
db/
  migrations/ 0001_init.up.sql / .down.sql   (format golang-migrate)
  seed/       dev_seed.sql                    (katalog, outlet, voucher)
  embed.go    SQL ikut ter-embed ke binary
internal/
  config/     baca env + .env
  httpx/      envelope, error, decode+validasi, paginasi, middleware (RequestID, Logger, Recover, APIKey, Auth, RateLimiter)
  token/      JWT access token + refresh token (SHA-256 di DB)
  database/   pgxpool, transaksi, migrator, seeder
  server/     router (semua route di satu tempat) + /healthz
  auth/       register, login, refresh (rotation + reuse detection), logout
  catalog/    banners, categories, products, stores
  order/      quote, checkout (transaksi + idempotency), list, detail, cancel, rating
  user/       me, addresses, favorites, vouchers, payment-methods (e-wallet tersimpan)
  home/       GET /v1/home (section Beranda dari tabel home_sections)
  appconfig/  GET /v1/app-config (tabel app_config) + order_rules untuk quote/checkout
  content/    GET /v1/help & /v1/legal/{slug} (Pusat Bantuan, Kebijakan Privasi)
  promo/      promo langganan: featured + activate
```

Pola per modul sama seperti iOS: **handler** (HTTP) → **service** (aturan bisnis) → **repository** (SQL), DTO JSON dipisah dari model.

## Endpoint

Semua di bawah `/v1` butuh header `X-API-Key`. 🔒 = butuh `Authorization: Bearer <access_token>`. 🔒* = token opsional (tanpa token tetap 200; token yang dikirim tapi kedaluwarsa/rusak tetap 401 supaya iOS refresh).

| Method | Path | |
|---|---|---|
| GET | `/healthz` | tanpa API key |
| POST | `/v1/auth/register` · `/login` · `/refresh` | login: rate limit 5/menit/IP+email |
| POST | `/v1/auth/logout` | 🔒 |
| GET | `/v1/banners` · `/categories` · `/products` · `/products/{id}` · `/stores?lat=&lng=` | |
| POST | `/v1/orders/quote` · `/v1/orders` | 🔒 `Idempotency-Key` opsional |
| GET | `/v1/orders?status=&page=&limit=` · `/v1/orders/{id}` | 🔒 |
| POST | `/v1/orders/{id}/cancel` · `/v1/orders/{id}/rating` | 🔒 |
| GET/PATCH | `/v1/me` | 🔒 |
| GET/POST | `/v1/me/addresses` · PATCH/DELETE `/v1/me/addresses/{id}` | 🔒 |
| GET | `/v1/me/favorites` · PUT/DELETE `/v1/me/favorites/{product_id}` | 🔒 |
| GET | `/v1/me/vouchers` | 🔒 |
| GET/POST | `/v1/me/payment-methods` | 🔒 · POST = tautkan e-wallet (`provider`, `account_number`), 409 `PAYMENT_METHOD_EXISTS` |
| PUT | `/v1/me/payment-methods/default` | 🔒 · body `{"id":"COD"\|"QRIS"\|<uuid>}` |
| DELETE | `/v1/me/payment-methods/{id}` | 🔒 · hanya e-wallet; bila metode utama → kembali ke COD |
| GET | `/v1/help?q=` | Pusat Bantuan: topik + FAQ + kontak (tabel `help_topics`, `help_articles`, key `help_contacts`) |
| GET | `/v1/legal/{slug}` | `privacy-policy` · `terms` (tabel `legal_documents`), `Cache-Control: max-age=3600` |
| GET | `/v1/home?lat=&lng=` | 🔒* |
| GET | `/v1/app-config` | `Cache-Control: max-age=300` + `ETag` (304 bila `If-None-Match` cocok) |
| GET | `/v1/promotions/featured` | 🔒* · `data: null` bila tidak ada promo |
| POST | `/v1/subscriptions/{plan_id}/activate` | 🔒 · 201, 409 `ALREADY_SUBSCRIBED` |
| POST | `/v1/dev/orders/{id}/status` | 🔒 **hanya `APP_ENV=dev`** |

### Mengatur Beranda & konfigurasi tanpa update app

Isi Beranda, aturan order, metode bayar, onboarding, dan menu Profil ada di database (nilai awalnya ikut migrasi `0002`, jadi staging/prod juga langsung punya). Contoh:

```sql
-- Ganti judul & jumlah produk "Paling Laris"
UPDATE home_sections SET title = '🔥 Terlaris Minggu Ini', item_limit = 8 WHERE id = 'best_seller';
-- Section manual
INSERT INTO home_sections (id, type, title, rule, product_ids, sort_order)
VALUES ('promo_kopi', 'PRODUCT_CAROUSEL', 'Promo Kopi', 'MANUAL', ARRAY['P04','P02'], 5);
-- Aktifkan gratis ongkir ≥ Rp 50.000 (quote/checkout di server ikut berubah)
UPDATE app_config SET value = jsonb_set(value, '{free_delivery_min_subtotal}', '50000') WHERE key = 'order_rules';
-- Aktifkan E-Wallet
UPDATE app_config SET value = (SELECT jsonb_agg(CASE WHEN m->>'id' = 'EWALLET' THEN m || '{"is_enabled":true}' ELSE m END)
  FROM jsonb_array_elements(value) m) WHERE key = 'payment_methods';
```

Key baru di `app_config` otomatis ikut di response `/v1/app-config`.

### Endpoint dev: simulasi status pesanan

Belum ada aplikasi outlet/kurir, jadi untuk mengetes tab *Diproses → Selesai → Beri Nilai* dari iOS:

```bash
curl -s localhost:8080/v1/dev/orders/<order_id>/status \
  -H 'X-API-Key: dev-local-api-key' -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"status":"PROCESSING"}'
# lalu DELIVERING → COMPLETED (mengikuti diagram status di plan)
```

## Penyesuaian dari plan

- **Query ditulis langsung dengan pgx**, belum pakai `sqlc`. Filter produk yang dinamis lebih sederhana begini, dan tidak perlu install CLI tambahan. Bisa dipindah ke sqlc nanti tanpa mengubah handler/service.
- **Migrasi lewat `go run ./cmd/migrate`**, tidak perlu install CLI `golang-migrate`. Nama file dan tabel `schema_migrations` tetap kompatibel dengan golang-migrate.
- **Router di `internal/server/`**, bukan `httpx/router.go`, supaya tidak ada import cycle (`httpx` dipakai oleh semua modul).
- **Voucher contoh** di seed: `HEMAT10` (10%, maks. Rp 20.000, min. Rp 30.000), `DISKON15K`, `KOPIPAGI`. Voucher sekali pakai per user, dan kembali bisa dipakai bila pesanan dibatalkan.
- **`store_id` boleh kosong** di quote/checkout: otomatis pakai outlet buka pertama (ST01), supaya iOS bisa checkout sebelum ada fitur pilih outlet.
- **Opsi produk**: produk yang punya varian tapi `option_id` kosong otomatis memakai varian pertama (Regular/Normal).
- **`order_rules` di `app_config` jadi sumber aturan order di server juga** (batas jumlah, panjang catatan, ongkir, gratis ongkir), jadi angka di app dan hasil quote/checkout selalu sama. `DELIVERY_FEE` / `FREE_DELIVERY_MIN_SUBTOTAL` di `.env` hanya cadangan bila key belum ada.
- **Gratis ongkir awalnya nonaktif** (`free_delivery_min_subtotal: 0`, contoh di plan 50000) supaya total server sama dengan Keranjang iOS yang masih hardcode ongkir. Aktifkan setelah iOS membaca `order_rules` (lihat SQL di atas).
- **Metode bayar yang `is_enabled: false`** ditolak saat checkout: `422 PAYMENT_METHOD_UNAVAILABLE` (EWALLET nonaktif secara default).
- **`/v1/home`**: section produk yang kosong tidak dikirim; `see_all` hanya ada di section produk. Tanpa `lat/lng`, `store` = outlet buka pertama (sama dengan default checkout).
- **Aktivasi langganan** langsung berhasil (belum ada pembayaran); berlaku 1 periode (DAILY = 24 jam, WEEKLY = 7 hari, MONTHLY = 1 bulan). Bisa diaktifkan lagi setelah kedaluwarsa.
- Error tambahan: `422 PAYMENT_METHOD_UNAVAILABLE`, `422 STORE_CLOSED`, `422 ORDER_NOT_CANCELLABLE`, `422 ORDER_NOT_RATABLE`, `409 ALREADY_RATED`, `422 INVALID_STATUS_TRANSITION` (dev).
- Detail pesanan menambahkan `voucher_code`, `rating`, `can_cancel`, `can_rate`; quote menambahkan `store_id` & `voucher_code`. iOS mengabaikan field yang tidak dikenal.
- `user_image` di login berisi `""` bila user belum punya avatar (bukan `null`), supaya aman untuk DTO iOS yang non-optional.

## Integrasi ke iOS

1. `make run`
2. `Configs/Dev.xcconfig` → `USE_MOCK_DATA = NO`, `API_KEY = dev-local-api-key`.
3. Scheme **TryTuist-Dev** di simulator (bisa langsung ke `localhost`). Device fisik: pakai IP Mac, mis. `http:/$()/192.168.1.10:8080`.
