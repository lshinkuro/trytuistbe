# Deploy TryTuist API ke IDCloudHost

Panduan dari nol sampai API jalan di `https://api.<domainmu>` dengan HTTPS, deploy otomatis dari GitHub, dan backup harian.

> Nama menu di panel IDCloudHost bisa sedikit berbeda dari yang tertulis di sini. Yang penting adalah **nilai** yang diisi.
> Semua perintah server ditulis untuk **Ubuntu 24.04 LTS**.

## Daftar isi

1. [Gambaran](#1-gambaran)
2. [Yang perlu disiapkan](#2-yang-perlu-disiapkan)
3. [Buat VPS di IDCloudHost](#3-buat-vps-di-idcloudhost)
4. [Arahkan domain](#4-arahkan-domain)
5. [Siapkan server](#5-siapkan-server)
6. [Isi secret di server](#6-isi-secret-di-server)
7. [Hubungkan GitHub Actions](#7-hubungkan-github-actions)
8. [Deploy pertama](#8-deploy-pertama)
9. [Setelah live](#9-setelah-live)
10. [Backup & restore](#10-backup--restore)
11. [Operasional harian](#11-operasional-harian)
12. [Troubleshooting](#12-troubleshooting)
13. [Checklist go-live](#13-checklist-go-live)

---

## 1. Gambaran

```
  iPhone / Web admin
         │  HTTPS 443
         ▼
┌──────────── VPS IDCloudHost (Ubuntu 24.04) ─────────────┐
│  caddy  ── sertifikat Let's Encrypt otomatis             │
│    │                                                     │
│    ▼ :8080 (jaringan internal Docker)                    │
│  api  ── image ghcr.io/lshinkuro/trytuistbe:<commit>     │
│    │                                                     │
│    ▼ :5432 (tidak dibuka ke internet)                    │
│  db   ── PostgreSQL 16, volume pgdata                    │
│                                                          │
│  /opt/trytuist/  .env.prod · backups/ · secrets/         │
└──────────────────────────────────────────────────────────┘
         ▲
         │ SSH (deploy.sh)
  GitHub Actions: test → build image → backup → migrasi → ganti api → cek /healthz
```

File yang dipakai ada di repo:

| File | Fungsi |
|---|---|
| `Dockerfile` | Build binary `api`, `migrate`, `seed` ke image distroless |
| `deploy/docker-compose.prod.yml` | Service `db`, `api`, `caddy`, dan `migrate` (dijalankan manual) |
| `deploy/Caddyfile` | Reverse proxy + HTTPS otomatis |
| `deploy/.env.prod.example` | Contoh semua secret |
| `deploy/deploy.sh` | Dijalankan CI di server: backup → migrasi → ganti api → cek kesehatan |
| `deploy/backup.sh` | Backup Postgres (cron harian + sebelum setiap deploy) |
| `.github/workflows/ci.yml` | Test di setiap PR; deploy di setiap push ke `main` |

## 2. Yang perlu disiapkan

- [ ] Akun [IDCloudHost](https://idcloudhost.com) dengan saldo
- [ ] Domain (boleh beli di IDCloudHost atau di tempat lain), mis. `trytuist.id` → API di `api.trytuist.id`
- [ ] Akses admin repo GitHub `lshinkuro/trytuistbe`
- [ ] Di laptop: `ssh`, `ssh-keygen`, `openssl` (sudah ada di macOS)
- [ ] (Nanti) Server key Midtrans, key APNs `.p8`, Google client ID

## 3. Buat VPS di IDCloudHost

1. Masuk ke panel IDCloudHost → **Cloud VPS** → **Buat / Create**.
2. Pilih:

   | Pilihan | Nilai | Catatan |
   |---|---|---|
   | Lokasi | Jakarta | Dekat dengan pengguna |
   | OS | **Ubuntu 24.04 LTS** | |
   | vCPU / RAM | **2 vCPU / 2 GB** | 1 GB masih bisa untuk uji, tapi build & Postgres lebih lega di 2 GB |
   | Disk | 40 GB | Data + backup 14 hari |
   | Public IP | **Ya** (IPv4 publik) | Wajib, untuk DNS & HTTPS |
   | Autentikasi | **SSH key** (lebih aman dari password) | Lihat langkah di bawah |
   | Nama | `trytuist-prod` | |

3. Buat SSH key **untuk kamu sendiri** (bila belum punya) dan tempel isi public key-nya ke panel:

   ```bash
   ssh-keygen -t ed25519 -C "kholis@laptop" -f ~/.ssh/trytuist_admin
   cat ~/.ssh/trytuist_admin.pub   # salin ke kolom SSH key di panel
   ```

4. Setelah VPS jadi, catat **IP publik**-nya. Contoh di panduan ini: `103.xx.xx.xx`.
5. Bila panel IDCloudHost punya **firewall / security group**, izinkan masuk hanya port **22, 80, 443** (TCP) dan **443 UDP** (HTTP/3). Firewall di dalam server (`ufw`) tetap dipasang di langkah 5.

Coba masuk:

```bash
ssh -i ~/.ssh/trytuist_admin root@103.xx.xx.xx
```

> User awal bisa `root` atau user lain sesuai template OS IDCloudHost. Lihat di halaman detail VPS.

## 4. Arahkan domain

Di pengelola DNS domainmu (menu **Domain → Kelola DNS** bila domain dibeli di IDCloudHost):

| Type | Name | Value | TTL |
|---|---|---|---|
| `A` | `api` | `103.xx.xx.xx` | 300 |
| `A` | `staging-api` *(opsional)* | IP VPS staging | 300 |

Cek dari laptop (tunggu beberapa menit):

```bash
dig +short api.trytuist.id    # harus keluar 103.xx.xx.xx
```

> DNS **harus sudah benar sebelum deploy pertama**. Caddy meminta sertifikat HTTPS ke Let's Encrypt saat start, dan permintaan itu gagal bila domain belum mengarah ke VPS.

## 5. Siapkan server

Semua perintah di bawah dijalankan **di VPS** sebagai `root` (atau dengan `sudo`).

### 5.1 Update, zona waktu, swap

```bash
apt update && apt -y upgrade
timedatectl set-timezone Asia/Jakarta

# Swap 2 GB: mencegah server mati kehabisan RAM saat build / backup
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

### 5.2 User `deploy` (dipakai GitHub Actions)

```bash
adduser --disabled-password --gecos "" deploy
mkdir -p /home/deploy/.ssh && chmod 700 /home/deploy/.ssh
# Salin key adminmu supaya kamu juga bisa masuk sebagai deploy
cp ~/.ssh/authorized_keys /home/deploy/.ssh/authorized_keys
chown -R deploy:deploy /home/deploy/.ssh && chmod 600 /home/deploy/.ssh/authorized_keys
```

### 5.3 Docker

```bash
curl -fsSL https://get.docker.com | sh
usermod -aG docker deploy
docker compose version   # pastikan plugin compose terpasang
```

### 5.4 Folder aplikasi

```bash
mkdir -p /opt/trytuist/backups /opt/trytuist/secrets
chown -R deploy:deploy /opt/trytuist
chmod 700 /opt/trytuist/secrets
```

### 5.5 Firewall & SSH

```bash
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw --force enable

# Matikan login password & login root langsung (pastikan login pakai key SUDAH berhasil)
sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/; s/^#\?PermitRootLogin .*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
systemctl restart ssh

apt -y install fail2ban unattended-upgrades   # blokir brute force SSH, patch keamanan otomatis
```

> ⚠️ Docker membuka port container **melewati ufw**. Karena itu `docker-compose.prod.yml` sengaja **tidak** mem-publish port Postgres. Jangan tambahkan `ports:` ke service `db`.

## 6. Isi secret di server

Login sebagai `deploy`, lalu buat `.env.prod` dari contoh di repo:

```bash
ssh -i ~/.ssh/trytuist_admin deploy@103.xx.xx.xx
cd /opt/trytuist
nano .env.prod      # tempel isi deploy/.env.prod.example, lalu isi semua nilai GANTI_…
chmod 600 .env.prod
```

Buat nilai acak (jalankan 3×, **bedakan** setiap nilai):

```bash
openssl rand -hex 32
```

Isi minimal:

| Variabel | Isi |
|---|---|
| `API_IMAGE` | `ghcr.io/lshinkuro/trytuistbe:latest` (diganti otomatis saat deploy) |
| `API_DOMAIN` | `api.trytuist.id` |
| `POSTGRES_PASSWORD` | nilai acak #1 |
| `DATABASE_URL` | `postgres://trytuist:<nilai acak #1>@db:5432/trytuist?sslmode=disable` |
| `API_KEYS` | nilai acak #2. **Sama dengan `API_KEY` di `Configs/Prod.xcconfig` iOS** |
| `JWT_SECRET` | nilai acak #3 (minimal 32 karakter) |
| `PUBLIC_BASE_URL` | `https://api.trytuist.id` |
| `APP_ENV` | `prod` (atau `staging` untuk server staging) |

- Server **menolak start** bila `API_KEYS` kosong atau masih nilai contoh (`…change-me`, `dev-local-api-key`). Ini disengaja.
- Mengganti `JWT_SECRET` nanti = semua user otomatis logout.
- Key APNs: salin file `.p8` ke server, lalu isi `APNS_KEY_FILE=/secrets/AuthKey_XXXX.p8`, `APNS_KEY_ID`, `APNS_TEAM_ID`:

  ```bash
  scp -i ~/.ssh/trytuist_admin AuthKey_XXXX.p8 deploy@103.xx.xx.xx:/opt/trytuist/secrets/
  ```

## 7. Hubungkan GitHub Actions

### 7.1 SSH key khusus CI

Buat key **baru** khusus untuk GitHub (jangan pakai key pribadimu):

```bash
# di laptop
ssh-keygen -t ed25519 -C "github-actions-deploy" -f ~/.ssh/trytuist_ci -N ""
ssh-copy-id -i ~/.ssh/trytuist_ci.pub -o "IdentityFile ~/.ssh/trytuist_admin" deploy@103.xx.xx.xx
```

### 7.2 Secret di GitHub

Repo → **Settings → Environments → New environment** → `production` → **Add environment secret**:

| Secret | Isi |
|---|---|
| `DEPLOY_HOST` | `103.xx.xx.xx` |
| `DEPLOY_USER` | `deploy` |
| `DEPLOY_SSH_KEY` | isi file **private** `~/.ssh/trytuist_ci` (seluruhnya, termasuk baris `-----BEGIN…`) |

> Opsional tapi disarankan: di environment `production` aktifkan **Required reviewers**. Deploy jadi menunggu persetujuanmu dulu.

### 7.3 Izin GHCR

Image disimpan di GitHub Container Registry. Workflow sudah meminta izin `packages: write` dan login ke GHCR memakai `GITHUB_TOKEN`, jadi tidak perlu token tambahan. Bila repo **private**, image juga private, dan server login otomatis setiap kali deploy berjalan.

## 8. Deploy pertama

1. Pastikan DNS (langkah 4) sudah mengarah ke VPS.
2. Push ke `main`, atau buka **Actions → CI / Deploy → Run workflow**.
3. Pantau job `deploy`. Urutannya:
   `build image` → `salin file deploy` → `backup.sh` → `migrate up` → `api` & `caddy` jalan → tunggu `https://api.trytuist.id/healthz`.
4. Cek dari laptop:

   ```bash
   curl https://api.trytuist.id/healthz
   # {"status":"ok","env":"prod","db":"ok"}
   ```

5. Buat **super admin pertama**. Seed diblokir di prod, jadi database awalnya kosong:

   ```bash
   # daftar akun biasa dulu (lewat app, atau curl)
   curl -s https://api.trytuist.id/v1/auth/register \
     -H "X-API-Key: <API_KEYS>" -H "Content-Type: application/json" \
     -d '{"email":"kamu@domain.com","password":"<password kuat>","name":"Kholis"}'

   # naikkan role-nya (di VPS)
   cd /opt/trytuist
   docker compose -f docker-compose.prod.yml --env-file .env.prod exec db \
     psql -U trytuist -d trytuist -c "UPDATE users SET role = 'SUPER_ADMIN' WHERE email = 'kamu@domain.com'"
   ```

6. Masuk ke web admin, lalu isi **outlet → kategori → menu**.

> Untuk server **staging**, boleh memakai data contoh:
> `docker compose -f docker-compose.prod.yml --env-file .env.prod run --rm --entrypoint /app/seed migrate`

## 9. Setelah live

**iOS** (`Configs/Prod.xcconfig`):

```
API_BASE_URL = https:/$()/api.trytuist.id
API_KEY      = <sama dengan API_KEYS di .env.prod>
USE_MOCK_DATA = NO
```

**Midtrans** (setelah akun diverifikasi):

- `.env.prod`: `MIDTRANS_SERVER_KEY=Mid-server-…`, `MIDTRANS_IS_PRODUCTION=true`
- Dashboard Midtrans produksi → Settings → Configuration → **Payment Notification URL**:
  `https://api.trytuist.id/v1/payments/midtrans/notification`
- Restart API supaya `.env.prod` baru terbaca:
  `docker compose -f docker-compose.prod.yml --env-file .env.prod up -d api`

**Email (Resend)** — kode verifikasi & reset password:

1. Daftar di [resend.com](https://resend.com) → **Domains → Add domain** (mis. `trytuist.id`), tambahkan record DNS (SPF, DKIM) yang diberikan di pengelola DNS, tunggu status *Verified*.
2. **API Keys → Create** (izin *Sending access*).
3. `.env.prod`: `RESEND_API_KEY=re_…`, `EMAIL_FROM=TryTuist <noreply@trytuist.id>`, lalu `dc up -d api`.

Tanpa domain terverifikasi, Resend hanya bisa mengirim ke email pemilik akun Resend (alamat `onboarding@resend.dev`).

**Swagger UI** (`/docs`) **sengaja mati di prod**. Pakai server staging atau lokal untuk membaca dokumentasi.

## 10. Backup & restore

### 10.1 Jadwal harian

Di VPS sebagai `deploy`:

```bash
crontab -e
# tambahkan baris ini (setiap hari 02:00 WIB):
0 2 * * * /opt/trytuist/backup.sh >> /opt/trytuist/backups/backup.log 2>&1
```

- Backup disimpan 14 hari di `/opt/trytuist/backups` (`BACKUP_KEEP_DAYS`).
- `deploy.sh` juga membuat backup **sebelum setiap migrasi**.
- Backup dicek utuh (gzip valid + baris penutup pg_dump) sebelum disimpan.

### 10.2 Salinan di luar VPS (sangat disarankan)

Backup yang hanya ada di VPS ikut hilang bila VPS rusak atau terhapus. Simpan salinannya di Object Storage (S3-compatible, mis. **Object Storage IDCloudHost**, Cloudflare R2, atau AWS S3):

```bash
sudo apt -y install rclone
rclone config            # n → nama "objstore" → pilih "s3" → provider "Other"
                         # isi access key, secret key, dan endpoint dari panel object storage
rclone mkdir objstore:trytuist-backup
```

Lalu di `.env.prod`: `BACKUP_RCLONE_REMOTE=objstore:trytuist-backup`

### 10.3 Restore

> Uji restore **minimal sekali sebulan** ke database terpisah. Backup yang belum pernah dicoba restore belum terbukti bisa dipakai.

```bash
cd /opt/trytuist
# Uji ke database terpisah (aman, data prod tidak tersentuh):
docker compose -f docker-compose.prod.yml --env-file .env.prod exec db createdb -U trytuist restore_test
gunzip -c backups/trytuist-YYYYMMDD-HHMMSS.sql.gz | \
  docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T db psql -U trytuist -d restore_test
docker compose -f docker-compose.prod.yml --env-file .env.prod exec db \
  psql -U trytuist -d restore_test -c "SELECT count(*) FROM orders"
docker compose -f docker-compose.prod.yml --env-file .env.prod exec db dropdb -U trytuist restore_test
```

Restore sungguhan (menimpa data prod): hentikan `api` dulu, restore ke `-d trytuist`, lalu jalankan lagi:

```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod stop api
gunzip -c backups/<file>.sql.gz | docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T db psql -U trytuist -d trytuist
docker compose -f docker-compose.prod.yml --env-file .env.prod start api
```

## 11. Operasional harian

Supaya perintah lebih pendek, tambahkan alias di `~/.bashrc` user `deploy`:

```bash
alias dc='docker compose -f /opt/trytuist/docker-compose.prod.yml --env-file /opt/trytuist/.env.prod'
```

| Kebutuhan | Perintah |
|---|---|
| Status container | `dc ps` |
| Log API (live) | `dc logs -f --tail=200 api` |
| Log HTTPS / sertifikat | `dc logs --tail=100 caddy` |
| Restart API (mis. setelah ubah `.env.prod`) | `dc up -d api` |
| Versi migrasi DB | `dc run --rm migrate version` |
| Masuk psql | `dc exec db psql -U trytuist -d trytuist` |
| Ubah aturan ongkir tanpa deploy | `UPDATE app_config …` di psql (lihat README) |
| Sisa disk | `df -h /` dan `docker system df` |
| Update OS | `sudo apt update && sudo apt -y upgrade` (reboot bila diminta) |

### Rollback ke versi sebelumnya

Setiap deploy memakai tag commit. Untuk kembali ke versi lama:

```bash
# di VPS, sebagai deploy. Login GHCR dulu bila repo private (PAT dengan scope read:packages):
echo <PAT> | docker login ghcr.io -u lshinkuro --password-stdin
/opt/trytuist/deploy.sh ghcr.io/lshinkuro/trytuistbe:<sha_commit_lama>
```

> ⚠️ Rollback hanya mengganti kode. **Migrasi database tidak dibatalkan otomatis.** Migrasi di repo ini hanya menambah kolom / tabel, jadi kode lama tetap jalan. Bila suatu saat ada migrasi yang menghapus kolom, rollback butuh restore backup.

## 12. Troubleshooting

| Gejala | Penyebab umum | Solusi |
|---|---|---|
| Deploy gagal di "tunggu /healthz", log Caddy berisi `challenge failed` | DNS belum mengarah ke VPS, atau port 80/443 tertutup | Cek `dig +short api.<domain>`, firewall panel IDCloudHost, dan `ufw status` |
| `api` restart terus, log: `API_KEYS … key contoh` | `.env.prod` masih memakai nilai contoh | Isi nilai acak, lalu `dc up -d api` |
| `api` log: `JWT_SECRET minimal 32 karakter` | Secret terlalu pendek | `openssl rand -hex 32` |
| `api` log: `password authentication failed` | `DATABASE_URL` tidak sama dengan `POSTGRES_PASSWORD` | Samakan keduanya. Catatan: password Postgres hanya dipakai saat volume **pertama kali** dibuat |
| iOS dapat `403 INVALID_API_KEY` | `API_KEY` di `Prod.xcconfig` ≠ `API_KEYS` server | Samakan |
| GitHub Actions: `ssh: handshake failed` | `DEPLOY_SSH_KEY` salah / public key belum di `authorized_keys` | Ulangi langkah 7.1 |
| `migrate` gagal saat deploy | Migrasi SQL error | API lama **tetap jalan**. Perbaiki migrasi, push lagi. Lihat log job |
| Webhook Midtrans tidak masuk | Notification URL salah / bukan https | Isi URL di §9, cek `dc logs api \| grep notification` |
| Disk penuh | Image lama / backup menumpuk | `docker image prune -a`, kurangi `BACKUP_KEEP_DAYS` |

## 13. Checklist go-live

- [ ] DNS `api.<domain>` → IP VPS, `curl https://api.<domain>/healthz` = `ok`
- [ ] Login SSH password dimatikan, `ufw` aktif (22/80/443 saja), fail2ban jalan
- [ ] `.env.prod` berisi nilai acak, `chmod 600`, **tidak** ada di git
- [ ] Super admin pertama dibuat, akun demo **tidak** ada di prod
- [ ] Cron backup terpasang, salinan ke object storage jalan, **restore sudah diuji**
- [ ] iOS `Prod.xcconfig`: `API_BASE_URL` & `API_KEY` sesuai, build Prod dicoba di device
- [ ] Midtrans produksi: server key + Notification URL; satu transaksi kecil sungguhan + refund berhasil
- [ ] APNs `.p8` terpasang, push diterima di device asli
- [ ] Environment `production` di GitHub memakai **Required reviewers**
