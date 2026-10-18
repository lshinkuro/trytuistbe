-- §5.24: harga promo per produk + popup promo di Beranda.
-- Selama deal aktif, harga produk di semua endpoint katalog DAN di quote/checkout
-- memakai deal_price (server tetap sumber kebenaran harga).
CREATE TABLE product_deals (
    id            TEXT PRIMARY KEY,
    product_id    TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    subtitle      TEXT NOT NULL DEFAULT '',
    badge         TEXT NOT NULL DEFAULT '',           -- mis. "HEMAT 38%"; kosong = dihitung app
    image_url     TEXT,                               -- NULL = pakai foto produk
    deal_price    BIGINT NOT NULL CHECK (deal_price > 0),
    starts_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at       TIMESTAMPTZ NOT NULL,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    show_as_popup BOOLEAN NOT NULL DEFAULT FALSE,     -- tampil sebagai popup di Beranda
    sort_order    INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

CREATE INDEX product_deals_active_idx ON product_deals (product_id) WHERE is_active;
CREATE INDEX product_deals_popup_idx ON product_deals (sort_order) WHERE is_active AND show_as_popup;
