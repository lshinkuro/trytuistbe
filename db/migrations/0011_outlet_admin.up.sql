-- Admin per outlet (§5.29): role bertingkat, stok & harga per outlet, promo per outlet.

-- Role: ADMIN lama menjadi SUPER_ADMIN; OUTLET_ADMIN wajib terikat ke satu outlet.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
UPDATE users SET role = 'SUPER_ADMIN' WHERE role = 'ADMIN';
ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('CUSTOMER', 'SUPER_ADMIN', 'OUTLET_ADMIN')),
    ADD COLUMN store_id TEXT REFERENCES stores(id),
    ADD CONSTRAINT users_outlet_admin_store CHECK ((role = 'OUTLET_ADMIN') = (store_id IS NOT NULL));

-- Menu yang dijual tiap outlet. Tidak ada baris = tidak dijual di outlet itu.
CREATE TABLE store_products (
    store_id     TEXT        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    product_id   TEXT        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    is_available BOOLEAN     NOT NULL DEFAULT TRUE,          -- saklar "habis" manual
    stock        INTEGER     CHECK (stock >= 0),              -- NULL = stok tidak dihitung
    price        BIGINT      CHECK (price > 0),               -- NULL = harga pusat (products.price)
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, product_id)
);
CREATE INDEX store_products_product_idx ON store_products (product_id);

-- Sebelumnya semua menu dijual di semua outlet; saklar habis dibawa dari tabel lama.
INSERT INTO store_products (store_id, product_id, is_available)
SELECT s.id, p.id, COALESCE(a.is_available, TRUE)
FROM stores s CROSS JOIN products p
LEFT JOIN store_product_availability a ON a.store_id = s.id AND a.product_id = p.id;

DROP TABLE store_product_availability;

-- Promo: NULL = semua outlet.
ALTER TABLE product_deals ADD COLUMN store_id TEXT REFERENCES stores(id) ON DELETE CASCADE;
CREATE INDEX product_deals_store_idx ON product_deals (store_id) WHERE is_active;
