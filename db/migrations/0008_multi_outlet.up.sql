-- §5.26: banyak outlet. Harga sama di semua outlet; yang berbeda per outlet adalah
-- stok (ketersediaan menu), jam buka, dan jangkauan antar.
ALTER TABLE stores
    ADD COLUMN phone              TEXT,
    ADD COLUMN opens_at           TIME         NOT NULL DEFAULT '00:00',   -- opens_at = closes_at → buka 24 jam
    ADD COLUMN closes_at          TIME         NOT NULL DEFAULT '00:00',   -- closes_at < opens_at → lewat tengah malam
    ADD COLUMN delivery_radius_km NUMERIC(4,1) NOT NULL DEFAULT 7
        CHECK (delivery_radius_km > 0 AND delivery_radius_km <= 50),
    ADD COLUMN is_active          BOOLEAN      NOT NULL DEFAULT TRUE,      -- FALSE = outlet ditutup permanen / disembunyikan
    ADD COLUMN sort_order         INTEGER      NOT NULL DEFAULT 0,
    ADD COLUMN created_at         TIMESTAMPTZ  NOT NULL DEFAULT now();
-- stores.is_open tetap dipakai sebagai tombol "tutup sementara" dari admin.

-- Ketersediaan menu per outlet. Tidak ada baris = ikut products.is_available.
-- products.is_available = FALSE berarti habis di semua outlet.
CREATE TABLE store_product_availability (
    store_id     TEXT        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    product_id   TEXT        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    is_available BOOLEAN     NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, product_id)
);

CREATE INDEX orders_store_created_idx ON orders (store_id, created_at DESC);
