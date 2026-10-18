-- §5.21–5.24: fitur Profil — metode pembayaran tersimpan, pusat bantuan, dokumen legal.
-- Konten bantuan & legal ada di migrasi (bukan seed) karena staging/prod juga butuh.
-- Ubah isinya lewat UPDATE/INSERT, tanpa deploy backend.

-- ========== Metode pembayaran ==========
-- COD & QRIS adalah metode bawaan (tidak disimpan per user). Yang disimpan hanya
-- akun e-wallet yang ditautkan user. Nomor kartu TIDAK pernah disimpan di sini.
CREATE TABLE user_payment_methods (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type            TEXT        NOT NULL DEFAULT 'EWALLET' CHECK (type IN ('EWALLET')),
    provider        TEXT        NOT NULL,                       -- id dari app_config.ewallet_providers
    account_number  TEXT        NOT NULL,                       -- nomor HP terdaftar di e-wallet (format 08…)
    account_name    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, provider, account_number)
);
CREATE INDEX user_payment_methods_user ON user_payment_methods (user_id, created_at);

-- 'COD' | 'QRIS' | id (uuid) dari user_payment_methods.
ALTER TABLE users ADD COLUMN default_payment_method TEXT NOT NULL DEFAULT 'COD';

-- E-wallet kini bisa dipakai checkout; daftar provider untuk form "Tambah e-wallet".
UPDATE app_config SET value = '[
    {"id":"COD","name":"Bayar di Tempat","icon":"banknote","description":"Bayar tunai ke kurir saat pesanan tiba","is_enabled":true},
    {"id":"QRIS","name":"QRIS","icon":"qrcode","description":"Scan QR dari aplikasi bank atau e-wallet apa pun","is_enabled":true},
    {"id":"EWALLET","name":"E-Wallet","icon":"wallet.pass","description":"GoPay, OVO, DANA, ShopeePay","is_enabled":true}
  ]', updated_at = now()
WHERE key = 'payment_methods';

INSERT INTO app_config (key, value) VALUES
  ('ewallet_providers', '[
    {"id":"GOPAY","name":"GoPay","icon":"wallet.pass.fill","tint":"teal"},
    {"id":"OVO","name":"OVO","icon":"wallet.pass.fill","tint":"purple"},
    {"id":"DANA","name":"DANA","icon":"wallet.pass.fill","tint":"blue"},
    {"id":"SHOPEEPAY","name":"ShopeePay","icon":"wallet.pass.fill","tint":"orange"}
  ]'),
  ('help_contacts', '{
    "operating_hours": "Setiap hari, 08.00–22.00 WIB",
    "channels": [
      {"type":"WHATSAPP","label":"Chat WhatsApp","value":"+62 812-1000-2000","icon":"message.fill","url":"https://wa.me/6281210002000"},
      {"type":"EMAIL","label":"Email","value":"halo@trytuist.com","icon":"envelope.fill","url":"mailto:halo@trytuist.com"},
      {"type":"PHONE","label":"Telepon","value":"1500-123","icon":"phone.fill","url":"tel:1500123"}
    ]
  }')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- Menu Profil diarahkan ke screen native (sebelumnya link web).
UPDATE app_config SET value = '[
    {"id":"edit_profile","title":"Edit Profil","icon":"person.crop.circle","tint":"orange","action":"trytuist://profile/edit"},
    {"id":"addresses","title":"Alamat Pengiriman","icon":"location.fill","tint":"blue","action":"trytuist://addresses"},
    {"id":"payment","title":"Metode Pembayaran","icon":"creditcard.fill","tint":"green","action":"trytuist://payment-methods"},
    {"id":"favorites","title":"Menu Favorit","icon":"heart.fill","tint":"pink","action":"trytuist://favorites"},
    {"id":"help","title":"Pusat Bantuan","icon":"questionmark.circle.fill","tint":"teal","action":"trytuist://help"},
    {"id":"language","title":"Bahasa","icon":"globe","tint":"indigo","action":"trytuist://language"},
    {"id":"privacy","title":"Kebijakan Privasi","icon":"lock.fill","tint":"gray","action":"trytuist://legal/privacy-policy"}
  ]', updated_at = now()
WHERE key = 'profile_menu';

-- ========== Pusat bantuan ==========
CREATE TABLE help_topics (
    id          TEXT PRIMARY KEY,                    -- 'order', 'payment'
    title       TEXT    NOT NULL,
    icon        TEXT    NOT NULL,                    -- SF Symbol
    sort_order  INTEGER NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE help_articles (
    id          TEXT PRIMARY KEY,                    -- 'order-cancel'
    topic_id    TEXT    NOT NULL REFERENCES help_topics(id) ON DELETE CASCADE,
    question    TEXT    NOT NULL,
    answer      TEXT    NOT NULL,                    -- teks biasa, paragraf dipisah baris kosong
    sort_order  INTEGER NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX help_articles_topic ON help_articles (topic_id, sort_order);

INSERT INTO help_topics (id, title, icon, sort_order) VALUES
  ('order',    'Pesanan',            'bag.fill',            1),
  ('payment',  'Pembayaran',         'creditcard.fill',     2),
  ('delivery', 'Pengiriman',         'bicycle',             3),
  ('account',  'Akun & Keamanan',    'person.badge.shield.checkmark.fill', 4),
  ('promo',    'Promo & Voucher',    'ticket.fill',         5);

INSERT INTO help_articles (id, topic_id, question, answer, sort_order) VALUES
  ('order-how',       'order',    'Bagaimana cara memesan?',
   E'Pilih menu di Beranda atau tab Menu, atur varian dan jumlah, lalu tekan "Tambah ke keranjang".\n\nBuka Keranjang, periksa pesanan, lalu tekan "Pesan Sekarang".', 1),
  ('order-cancel',    'order',    'Bisakah saya membatalkan pesanan?',
   E'Bisa, selama status pesanan masih "Menunggu". Buka tab Pesanan, pilih pesanan, lalu tekan "Batalkan".\n\nPesanan yang sudah diproses outlet tidak bisa dibatalkan.', 2),
  ('order-status',    'order',    'Apa arti status pesanan?',
   E'Menunggu: pesanan diterima dan menunggu konfirmasi outlet.\nDiproses: pesanan sedang disiapkan.\nDiantar: kurir sedang menuju alamatmu.\nSelesai: pesanan sudah diterima.', 3),
  ('payment-methods', 'payment',  'Metode pembayaran apa saja yang tersedia?',
   E'Bayar di Tempat (COD), QRIS, dan e-wallet (GoPay, OVO, DANA, ShopeePay).\n\nAtur metode utama di Profil > Metode Pembayaran.', 1),
  ('payment-ewallet', 'payment',  'Bagaimana cara menautkan e-wallet?',
   E'Buka Profil > Metode Pembayaran > Tambah E-Wallet. Pilih penyedia dan masukkan nomor HP yang terdaftar di e-wallet tersebut.', 2),
  ('payment-refund',  'payment',  'Kapan dana saya dikembalikan?',
   E'Untuk pesanan non-tunai yang dibatalkan, dana kembali ke metode pembayaran asal dalam 1–3 hari kerja.', 3),
  ('delivery-fee',    'delivery', 'Berapa ongkos kirimnya?',
   E'Ongkos kirim standar Rp 10.000. Nikmati gratis ongkir untuk pesanan minimal Rp 50.000 saat promo berlangsung.', 1),
  ('delivery-time',   'delivery', 'Berapa lama pesanan sampai?',
   E'Rata-rata 20–40 menit, tergantung jarak outlet dan antrean pesanan.', 2),
  ('delivery-address','delivery', 'Bagaimana cara mengganti alamat pengiriman?',
   E'Buka Profil > Alamat Pengiriman. Tambah alamat baru atau pilih "Jadikan utama" pada alamat yang ingin dipakai.', 3),
  ('account-edit',    'account',  'Bagaimana cara mengubah nama atau nomor HP?',
   E'Buka Profil lalu tekan "Ubah" di kartu profil. Simpan setelah selesai mengubah data.', 1),
  ('account-security','account',  'Akun saya keluar sendiri, kenapa?',
   E'Demi keamanan, sesi login berakhir otomatis setelah beberapa waktu. Masuk kembali dengan email dan password kamu.', 2),
  ('promo-voucher',   'promo',    'Bagaimana cara memakai voucher?',
   E'Voucher yang kamu punya ada di Profil > Voucher. Pilih voucher saat checkout; potongan akan terlihat di ringkasan pembayaran.', 1);

-- ========== Dokumen legal ==========
CREATE TABLE legal_documents (
    slug          TEXT PRIMARY KEY,                  -- 'privacy-policy', 'terms'
    title         TEXT        NOT NULL,
    version       TEXT        NOT NULL,
    summary       TEXT,
    sections      JSONB       NOT NULL,              -- [{"heading": "...", "body": "..."}]
    effective_at  DATE        NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(sections) = 'array')
);

INSERT INTO legal_documents (slug, title, version, summary, effective_at, sections) VALUES
  ('privacy-policy', 'Kebijakan Privasi', '1.0',
   'Kami hanya mengumpulkan data yang dibutuhkan untuk memproses pesananmu, dan tidak pernah menjual data pribadimu.',
   '2026-10-01', '[
    {"heading":"1. Data yang kami kumpulkan","body":"Nama, email, nomor HP, alamat pengiriman, riwayat pesanan, dan nomor HP e-wallet yang kamu tautkan. Kami tidak menyimpan nomor kartu kredit/debit."},
    {"heading":"2. Cara kami memakai data","body":"Untuk memproses dan mengantar pesanan, menghubungimu soal pesanan, memberi promo yang relevan, serta menjaga keamanan akun."},
    {"heading":"3. Berbagi data","body":"Data hanya dibagikan ke outlet dan kurir yang menangani pesananmu, serta penyedia pembayaran saat transaksi. Kami tidak menjual data pribadimu ke pihak lain."},
    {"heading":"4. Penyimpanan & keamanan","body":"Password disimpan dalam bentuk hash. Akses ke data dibatasi dan dicatat. Data disimpan selama akunmu aktif atau sesuai kewajiban hukum."},
    {"heading":"5. Hak kamu","body":"Kamu bisa melihat dan mengubah data di menu Profil, menghapus alamat dan e-wallet tersimpan, atau meminta penghapusan akun lewat Pusat Bantuan."},
    {"heading":"6. Hubungi kami","body":"Pertanyaan soal privasi: halo@trytuist.com."}
   ]'),
  ('terms', 'Syarat & Ketentuan', '1.0',
   'Ketentuan penggunaan aplikasi TryTuist.',
   '2026-10-01', '[
    {"heading":"1. Akun","body":"Kamu bertanggung jawab menjaga kerahasiaan password akunmu."},
    {"heading":"2. Pesanan","body":"Harga dan ketersediaan menu dapat berubah sewaktu-waktu. Total akhir mengikuti perhitungan saat checkout."},
    {"heading":"3. Pembatalan","body":"Pesanan hanya bisa dibatalkan selama berstatus Menunggu."}
   ]');
