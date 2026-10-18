-- §5.18–5.20: Beranda dinamis, konfigurasi app, promo langganan.

-- Isi & urutan section Beranda, diatur dari backend
CREATE TABLE home_sections (
    id           TEXT PRIMARY KEY,                    -- 'best_seller', 'recommended'
    type         TEXT    NOT NULL CHECK (type IN ('BANNER_CAROUSEL','CATEGORY_GRID','PRODUCT_CAROUSEL','PRODUCT_LIST')),
    title        TEXT,
    rule         TEXT    CHECK (rule IN ('BEST_SELLER','TOP_RATED','MANUAL')),   -- khusus section produk
    product_ids  TEXT[],                              -- dipakai bila rule = 'MANUAL'
    item_limit   INTEGER NOT NULL DEFAULT 6 CHECK (item_limit BETWEEN 1 AND 50),
    see_all      JSONB,                               -- mis. {"sort":"popular"}
    sort_order   INTEGER NOT NULL DEFAULT 0,
    is_active    BOOLEAN NOT NULL DEFAULT TRUE,
    CHECK (type NOT IN ('PRODUCT_CAROUSEL','PRODUCT_LIST') OR rule IS NOT NULL)
);

-- Konfigurasi app (satu baris per key, nilai JSON)
CREATE TABLE app_config (
    key         TEXT PRIMARY KEY,                     -- 'order_rules', 'onboarding', 'profile_menu', ...
    value       JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Paket langganan & langganan user
CREATE TABLE subscription_plans (
    id          TEXT PRIMARY KEY,                     -- 'daily-coffee'
    eyebrow     TEXT    NOT NULL,
    title       TEXT    NOT NULL,
    price       BIGINT  NOT NULL CHECK (price >= 0),
    period      TEXT    NOT NULL CHECK (period IN ('DAILY','WEEKLY','MONTHLY')),
    image_url   TEXT,
    is_featured BOOLEAN NOT NULL DEFAULT FALSE,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE user_subscriptions (
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id       TEXT NOT NULL REFERENCES subscription_plans(id),
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    active_until  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, plan_id)
);

-- Nilai awal (sama dengan tampilan iOS sekarang). Ada di migrasi, bukan seed,
-- karena staging/prod juga membutuhkannya. Ubah lewat UPDATE, tanpa deploy.
INSERT INTO home_sections (id, type, title, rule, item_limit, see_all, sort_order) VALUES
  ('promo',       'BANNER_CAROUSEL',  NULL,                     NULL,          5, NULL,                   1),
  ('categories',  'CATEGORY_GRID',    'Kategori',               NULL,         12, NULL,                   2),
  ('best_seller', 'PRODUCT_CAROUSEL', '🔥 Paling Laris',        'BEST_SELLER', 6, '{"sort":"popular"}',   3),
  ('recommended', 'PRODUCT_LIST',     'Rekomendasi Buat Kamu',  'TOP_RATED',   5, NULL,                   4);

INSERT INTO subscription_plans (id, eyebrow, title, price, period, is_featured) VALUES
  ('daily-coffee', 'LANGGANAN KOPI HARIAN', '1 cup cuma 15rb / hari', 15000, 'DAILY', TRUE);

INSERT INTO app_config (key, value) VALUES
  ('min_supported_version', '"1.0.0"'),
  ('latest_version',        '"1.0.0"'),
  ('search_placeholder',    '"Cari kopi, nasi goreng, burger…"'),
  -- order_rules juga dipakai server saat menghitung quote/checkout,
  -- jadi angka di app dan di server selalu sama.
  -- free_delivery_min_subtotal = 0 (nonaktif) supaya total server sama dengan
  -- Keranjang iOS yang masih hardcode ongkir; set 50000 setelah iOS membaca order_rules.
  ('order_rules', '{"min_quantity":1,"max_quantity":20,"max_note_length":100,"delivery_fee":10000,"free_delivery_min_subtotal":0}'),
  ('payment_methods', '[
    {"id":"COD","name":"Bayar di Tempat","icon":"banknote","is_enabled":true},
    {"id":"QRIS","name":"QRIS","icon":"qrcode","is_enabled":true},
    {"id":"EWALLET","name":"E-Wallet","icon":"wallet.pass","is_enabled":false}
  ]'),
  ('onboarding', '[
    {"image_url":"https://images.unsplash.com/photo-1512058564366-18510be2db19?w=800&q=80&auto=format&fit=crop",
     "title":"Semua favoritmu di sini","subtitle":"Kopi, nasi goreng, burger, sampai dessert. Tinggal pilih."},
    {"image_url":"https://images.unsplash.com/photo-1767471716700-2b3aa77b1ab6?w=800&q=80&auto=format&fit=crop",
     "title":"Pesan cepat tanpa antre","subtitle":"Atur varian, jumlah, dan catatan sesuai seleramu."},
    {"image_url":"https://images.unsplash.com/photo-1543339469-94ba2391431f?w=800&q=80&auto=format&fit=crop",
     "title":"Diantar hangat ke rumah","subtitle":"Nikmati promo dan gratis ongkir setiap hari."}
  ]'),
  ('profile_menu', '[
    {"id":"addresses","title":"Alamat Pengiriman","icon":"location.fill","tint":"blue","action":"trytuist://addresses"},
    {"id":"payment","title":"Metode Pembayaran","icon":"creditcard.fill","tint":"green","action":"trytuist://payment-methods"},
    {"id":"favorites","title":"Menu Favorit","icon":"heart.fill","tint":"pink","action":"trytuist://favorites"},
    {"id":"help","title":"Pusat Bantuan","icon":"questionmark.circle.fill","tint":"teal","action":"https://trytuist.com/bantuan"},
    {"id":"language","title":"Bahasa","icon":"globe","tint":"indigo","action":"trytuist://language"},
    {"id":"privacy","title":"Kebijakan Privasi","icon":"lock.fill","tint":"gray","action":"https://trytuist.com/privasi"}
  ]');
