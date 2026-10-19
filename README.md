# TryTuist API (Go + PostgreSQL)

Backend untuk aplikasi iOS **TryTuist**. Kontraknya ada di [`API_PLAN.md`](API_PLAN.md); semua nama field JSON sama dengan DTO di iOS.

Semua endpoint di plan (#1–#50, fase 1 sampai 5c) sudah jalan dan dites, termasuk halaman Pembayaran & refund admin (fase 5b). Deploy produksi: lihat [Deploy ke VPS](#deploy-ke-vps).

## Jalankan di lokal (Postgres terinstal langsung di Mac)

Butuh **Go 1.23+** dan **PostgreSQL 13+** yang sedang berjalan (`brew services start postgresql@16` atau Postgres.app).

```bash
cd ~/Documents/iOS/trytuist-api
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

Akun demo (sama dengan `AuthMockDataSource`): `kholis@gmail.com / 12345678` dan `demo@trytuist.com / password123`. Akun web admin: `admin@trytuist.com / admin12345` (`SUPER_ADMIN`) dan `kemang@trytuist.com / outlet12345` (`OUTLET_ADMIN` Kemang Utara, §5.29).

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
docs/         openapi.yaml + Swagger UI di /docs
deploy/       Docker Compose produksi, Caddy, deploy.sh, backup.sh
```

Pola per modul sama seperti iOS: **handler** (HTTP) → **service** (aturan bisnis) → **repository** (SQL), DTO JSON dipisah dari model.

## Endpoint

**Swagger UI:** buka [http://localhost:8080/docs](http://localhost:8080/docs) setelah `make run` (juga di staging; dimatikan di prod). Spec-nya `docs/openapi.yaml` (OpenAPI 3), bisa diimpor ke Postman / Insomnia. Klik **Authorize** → isi `X-API-Key`, panggil `POST /v1/auth/login`, lalu tempel `access_token` ke `bearerAuth`.
Menambah atau mengubah endpoint wajib memperbarui `docs/openapi.yaml`: `TestOpenAPICoversRoutes` gagal bila ada route yang belum tertulis (atau path di spec yang sudah tidak ada).

Semua di bawah `/v1` butuh header `X-API-Key`. 🔒 = butuh `Authorization: Bearer <access_token>`. 🔒* = token opsional (tanpa token tetap 200; token yang dikirim tapi kedaluwarsa/rusak tetap 401 supaya iOS refresh).

| Method | Path | |
|---|---|---|
| GET | `/healthz` | tanpa API key |
| POST | `/v1/auth/register` · `/login` · `/refresh` | login: rate limit 5/menit/IP+email |
| POST | `/v1/auth/logout` | 🔒 |
| POST | `/v1/auth/google` | daftar/masuk dengan Google (`id_token`). 201 = akun baru, 200 = akun lama. Butuh `GOOGLE_CLIENT_IDS` |
| GET | `/v1/banners` · `/categories` · `/products` · `/products/{id}` · `/stores?lat=&lng=` | produk: `?store_id=` → stok outlet itu · stores: buka → menjangkau → terdekat |
| POST | `/v1/orders/quote` · `/v1/orders` | 🔒 `Idempotency-Key` opsional |
| POST | `/v1/checkout` | 🔒 `Idempotency-Key` · COD → pesanan; Bayar Online → sesi Midtrans Snap (`redirect_url`), pesanan dibuat setelah lunas (§5.27) |
| GET / POST | `/v1/payments?status=` · `/v1/payments/{id}` · `/v1/payments/{id}/cancel` | 🔒 · status sesi (server ikut cek ke Midtrans saat PENDING), batal |
| POST | `/v1/payments/midtrans/notification` | **tanpa** `X-API-Key`; webhook Midtrans, divalidasi `signature_key` |
| PUT | `/v1/me/devices` | 🔒 · daftarkan device token APNs (§5.28) |
| POST | `/v1/devices/unregister` | tanpa login · berhenti menerima push (logout) |
| GET | `/v1/orders?status=&from=&to=&sort=asc/desc&page=&limit=` · `/v1/orders/{id}` | 🔒 · list berisi `can_rate` |
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
| GET | `/v1/home?lat=&lng=&store_id=` | 🔒* |
| GET | `/v1/app-config` | `Cache-Control: max-age=300` + `ETag` (304 bila `If-None-Match` cocok) |
| GET | `/v1/promotions/popup` | popup promo harga produk di Beranda (tabel `product_deals`) · `data: null` bila tidak ada |
| GET | `/v1/promotions/featured` | 🔒* · `data: null` bila tidak ada promo |
| POST | `/v1/subscriptions/{plan_id}/activate` | 🔒 · 201, 409 `ALREADY_SUBSCRIBED` |
| POST | `/v1/dev/orders/{id}/status` | 🔒 **hanya `APP_ENV=dev`** |
| GET | `/v1/admin/me` · `/dashboard` · `/orders[/{id}]` · `/products[/{id}]` · `/deals` · `/customers` · `/audit-logs` | 🛡 `SUPER_ADMIN` / `OUTLET_ADMIN` (web admin `trytuistadmin`, §5.25, §5.29); admin outlet dipaksa ke outletnya |
| PATCH | `/v1/admin/orders/{id}/status` · `/v1/admin/products/{id}` · `/v1/admin/deals/{id}` | 🛡 · ubah status (sukses/gagal/batal), harga & stok, promo |
| POST | `/v1/admin/products` | 🛡 · tambah menu (kategori, harga, foto, varian) → 201 |
| POST / DELETE | `/v1/admin/deals[/{id}]` | 🛡 · buat / hapus promo |
| GET / POST / PATCH | `/v1/admin/stores[/{id}]` | 🛡 · outlet: tambah, jam buka, radius antar, tutup sementara (§5.26) |
| PUT | `/v1/admin/stores/{id}/products/{product_id}` | 🛡 · `{ is_listed, is_available, stock, price }` menu, stok & harga per outlet (§5.29) |
| GET | `/v1/admin/payments?status=&q=&from=&to=&store_id=&refund=1` · `/v1/admin/payments/{id}` | 🛡 · sesi Midtrans, riwayat refund & event; admin outlet hanya outletnya |
| POST | `/v1/admin/payments/{id}/refund` | 🛡 `SUPER_ADMIN` · `{ amount, reason, manual }` refund penuh / sebagian lewat Midtrans, atau catat refund manual (VA / gerai) (§5.27.11) |
| GET / POST / PATCH | `/v1/admin/categories[/{id}]` · `/v1/admin/staff[/{id}]` | 🛡 baca kategori; tulis kategori & akun admin khusus `SUPER_ADMIN` |

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
- **Notifikasi push (§5.28)**: isi `APNS_KEY_ID`, `APNS_TEAM_ID`, `APNS_KEY_FILE` (file `.p8` dari developer.apple.com → Keys). Tanpa itu, push hanya dicatat di log (`push (APNs belum dikonfigurasi, hanya log)`). Tes: `POST /v1/dev/push-test` dengan token login.
- **Pembayaran online (§5.27)**: isi `MIDTRANS_SERVER_KEY` (sandbox `SB-Mid-server-…`) di `.env` untuk mengaktifkan "Bayar Online". Webhook Midtrans butuh URL publik: jalankan `cloudflared tunnel --url http://localhost:8080` dan isi Payment Notification URL di dashboard sandbox dengan `https://<tunnel>/v1/payments/midtrans/notification`. Tanpa tunnel pun status tetap terbaca karena server mengecek ke Midtrans setiap iOS polling. Tanpa simulator sama sekali: `POST /v1/dev/payments/{id}/simulate {"status":"settlement"}` (hanya `APP_ENV=dev`; di iOS lewat shake → Debugger → Navigasi → **Simulasi Midtrans**). Bayar di sandbox pakai simulator (`https://simulator.sandbox.midtrans.com/v2/qris/index`, kartu `4811 1111 1111 1114` / CVV `123` / OTP `112233`).
- **Multi-outlet (§5.26)**: harga sama di semua outlet; stok, jam buka, dan radius antar per outlet. `store_id` boleh kosong di quote/checkout: server memilih outlet yang buka, menjangkau alamat pesanan, lalu terdekat. Outlet tutup → `422 STORE_CLOSED`, alamat di luar radius → `422 OUT_OF_DELIVERY_AREA`. Seed: ST01 & ST02 buka 24 jam, ST03 Kelapa Gading 10:00–22:00; Ramen Pedas habis di ST02, Chocolate Cake habis di ST03.
- **Opsi produk**: produk yang punya varian tapi `option_id` kosong otomatis memakai varian pertama (Regular/Normal).
- **`order_rules` di `app_config` jadi sumber aturan order di server juga** (batas jumlah, panjang catatan, ongkir, gratis ongkir), jadi angka di app dan hasil quote/checkout selalu sama. `DELIVERY_FEE` / `FREE_DELIVERY_MIN_SUBTOTAL` di `.env` hanya cadangan bila key belum ada.
- **Gratis ongkir awalnya nonaktif** (`free_delivery_min_subtotal: 0`, contoh di plan 50000) supaya total server sama dengan Keranjang iOS yang masih hardcode ongkir. Aktifkan setelah iOS membaca `order_rules` (lihat SQL di atas).
- **Metode bayar yang `is_enabled: false`** ditolak saat checkout: `422 PAYMENT_METHOD_UNAVAILABLE` (EWALLET nonaktif secara default).
- **`/v1/home`**: section produk yang kosong tidak dikirim; `see_all` hanya ada di section produk. `?store_id=` memakai outlet itu (stok ikut outlet); tanpa itu, `store` = outlet terbaik untuk `lat/lng` (sama dengan default checkout).
- **Aktivasi langganan** langsung berhasil (belum ada pembayaran); berlaku 1 periode (DAILY = 24 jam, WEEKLY = 7 hari, MONTHLY = 1 bulan). Bisa diaktifkan lagi setelah kedaluwarsa.
- Error tambahan: `422 PAYMENT_METHOD_UNAVAILABLE`, `422 STORE_CLOSED`, `422 ORDER_NOT_CANCELLABLE`, `422 ORDER_NOT_RATABLE`, `409 ALREADY_RATED`, `422 INVALID_STATUS_TRANSITION` (dev).
- Detail pesanan menambahkan `voucher_code`, `rating`, `can_cancel`, `can_rate`; quote menambahkan `store_id` & `voucher_code`. iOS mengabaikan field yang tidak dikenal.
- `user_image` di login berisi `""` bila user belum punya avatar (bukan `null`), supaya aman untuk DTO iOS yang non-optional.

### Ongkir per jarak & estimasi waktu antar

Quote & pesanan berisi `distance_km`, `eta_min_minutes`, `eta_max_minutes` (null bila alamat tanpa koordinat). Aturannya di `app_config.order_rules` (migrasi `0012`), bisa diubah tanpa deploy:

```sql
-- Rp 8.000 sampai 2 km, + Rp 2.500/km, maks. Rp 25.000; siapkan 10 menit, kurir 25 km/jam
UPDATE app_config SET value = value || '{"delivery_fee":8000,"delivery_base_km":2,"delivery_fee_per_km":2500,"delivery_fee_max":25000,"prep_minutes":10,"courier_speed_kmh":25}'
WHERE key = 'order_rules';
-- Kembali ke ongkir flat
UPDATE app_config SET value = value || '{"delivery_fee_per_km":0}' WHERE key = 'order_rules';
```

## Uji Midtrans sandbox

Kode & tes otomatis sudah ada (fake Midtrans). Yang perlu dilakukan manual sekali dengan akun sandbox asli:

1. Daftar di [dashboard.sandbox.midtrans.com](https://dashboard.sandbox.midtrans.com) → Settings → Access Keys → salin **Server Key** (`SB-Mid-server-…`) ke `.env` sebagai `MIDTRANS_SERVER_KEY`.
2. Settings → Payment → aktifkan GoPay, QRIS, ShopeePay, VA (BCA/BNI/BRI), kartu.
3. `cloudflared tunnel --url http://localhost:8080` → isi `PUBLIC_BASE_URL` dengan URL tunnel, dan Settings → Configuration → **Payment Notification URL** = `https://<tunnel>/v1/payments/midtrans/notification`. `make run`.
4. Dari app iOS (scheme Dev, `USE_MOCK_DATA = NO`) checkout "Bayar Online", lalu cek tiap baris di bawah (simulator & kartu uji ada di API_PLAN §5.27.10):

| Skenario | Cara | Hasil yang diharapkan |
|---|---|---|
| GoPay / QRIS | Bayar lewat simulator QRIS/deeplink | App: "Pembayaran berhasil"; admin → Pembayaran: Lunas + pesanan; `payment_events` berisi `webhook` |
| VA BCA | Bayar nomor VA di simulator BCA | Sama seperti di atas; refund otomatis ditolak → catat manual |
| Kartu 3DS | `4811 1111 1111 1114`, CVV `123`, OTP `112233` | Lunas |
| Kedaluwarsa | `PAYMENT_EXPIRY=2m`, jangan bayar | Status Kedaluwarsa ±2–4 menit kemudian, push "Waktu pembayaran habis" |
| Batal | Tutup halaman bayar → Batalkan | Dibatalkan; VA lama tidak bisa dibayar |
| Refund sebagian & penuh | Web admin → Pembayaran → detail → Refund (GoPay / kartu) | Direfund sebagian → penuh; muncul juga di dashboard Midtrans |
| Webhook mati | Matikan tunnel lalu bayar | Tetap Lunas lewat polling app (sedikit lebih lambat) |

## Deploy ke VPS

> Panduan langkah demi langkah untuk IDCloudHost (buat VPS, DNS, hardening server, secret, GitHub Actions, backup, rollback, troubleshooting): **[DEPLOY.md](DEPLOY.md)**. Bagian di bawah ini ringkasannya.

Satu VPS (Ubuntu, 1 vCPU / 1–2 GB cukup untuk awal) menjalankan Postgres + API + Caddy (HTTPS otomatis). File ada di `deploy/`, pipeline di `.github/workflows/ci.yml`.

**Sekali di awal (di server):**

```bash
# 1. Docker + compose plugin
curl -fsSL https://get.docker.com | sh
sudo mkdir -p /opt/trytuist/secrets /opt/trytuist/backups && sudo chown -R $USER /opt/trytuist

# 2. Secret: salin deploy/.env.prod.example ke /opt/trytuist/.env.prod, isi semua GANTI_…
openssl rand -hex 32   # untuk POSTGRES_PASSWORD, API_KEYS, JWT_SECRET (bedakan tiap nilai)
chmod 600 /opt/trytuist/.env.prod

# 3. DNS: A record API_DOMAIN → IP VPS. Firewall: buka 22, 80, 443 saja.

# 4. Backup harian 02:00
(crontab -l 2>/dev/null; echo "0 2 * * * /opt/trytuist/backup.sh >> /opt/trytuist/backups/backup.log 2>&1") | crontab -
```

**Di GitHub** → Settings → Environments → `production` → secrets: `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY` (private key yang public key-nya ada di `~/.ssh/authorized_keys` server).

**Setiap push ke `main`:** `make lint` + `make test` (Postgres service, semua migrasi dijalankan ke DB kosong) → build image `ghcr.io/<repo>:<sha>` → di server `deploy.sh`: backup → `migrate up` → ganti container `api` → tunggu `https://API_DOMAIN/healthz`. Migrasi gagal = API lama tetap jalan. 

**Database prod pertama kali kosong** (`cmd/seed` diblokir di prod karena membuat akun demo). Buat super admin pertama: daftar akun biasa lewat app / `POST /v1/auth/register`, lalu:

```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod exec db \
  psql -U trytuist -d trytuist -c "UPDATE users SET role = 'SUPER_ADMIN' WHERE email = 'kamu@domain.com'"
```

Setelah itu outlet, kategori, dan menu diisi dari web admin. Di staging boleh memakai data contoh: `… run --rm --entrypoint /app/seed migrate`.

**Restore** (uji minimal sekali sebulan ke DB terpisah):

```bash
gunzip -c backups/trytuist-YYYYMMDD-HHMMSS.sql.gz | docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T db psql -U trytuist -d trytuist
```

- `API_KEYS` di staging/prod wajib diisi dan tidak boleh key contoh (`…change-me` / `dev-local-api-key`), server menolak start. Samakan `API_KEY` di `Configs/Prod.xcconfig` iOS.
- Backup yang hanya ada di VPS ikut hilang bila VPS rusak: isi `BACKUP_RCLONE_REMOTE` (mis. Cloudflare R2 / Google Drive lewat `rclone config`).
- Web admin (`trytuistadmin`) belum termasuk compose ini.

## Integrasi ke iOS

1. `make run`
2. `Configs/Dev.xcconfig` → `USE_MOCK_DATA = NO`, `API_KEY = dev-local-api-key`.
3. Scheme **TryTuist-Dev** di simulator (bisa langsung ke `localhost`). Device fisik: pakai IP Mac, mis. `http:/$()/192.168.1.10:8080`.

## Login dengan Google

1. Google Cloud Console → **APIs & Services → Credentials → Create credentials → OAuth client ID → iOS**. Isi Bundle ID app (`com.lshinkuro.TryTuist.dev` untuk Dev, `.staging`, dan tanpa akhiran untuk Prod).
2. Salin **Client ID** ke `.env` backend: `GOOGLE_CLIENT_IDS=<client-id-dev>,<client-id-prod>` lalu restart server.
3. Isi Client ID yang sama di iOS (`Configs/<Env>.xcconfig` → `GOOGLE_CLIENT_ID` & `GOOGLE_REVERSED_CLIENT_ID`).

Backend memverifikasi `id_token` sendiri (tanda tangan RS256 dengan kunci publik Google, `aud` = salah satu `GOOGLE_CLIENT_IDS`, `email_verified`), tanpa library tambahan. Email yang sudah terdaftar dengan password otomatis ditautkan ke Google.
