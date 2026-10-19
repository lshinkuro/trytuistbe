DROP INDEX IF EXISTS product_deals_store_idx;
DELETE FROM product_deals WHERE store_id IS NOT NULL;
ALTER TABLE product_deals DROP COLUMN IF EXISTS store_id;

CREATE TABLE store_product_availability (
    store_id     TEXT        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    product_id   TEXT        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    is_available BOOLEAN     NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, product_id)
);
-- Stok & harga per outlet hilang; menu yang tidak dijual / stok 0 jadi "habis".
INSERT INTO store_product_availability (store_id, product_id, is_available)
SELECT s.id, p.id, FALSE
FROM stores s CROSS JOIN products p
LEFT JOIN store_products sp ON sp.store_id = s.id AND sp.product_id = p.id
WHERE sp.product_id IS NULL OR NOT sp.is_available OR sp.stock = 0;
DROP TABLE store_products;

UPDATE users SET store_id = NULL, role = 'CUSTOMER' WHERE role = 'OUTLET_ADMIN';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_outlet_admin_store, DROP COLUMN IF EXISTS store_id;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
UPDATE users SET role = 'ADMIN' WHERE role = 'SUPER_ADMIN';
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('CUSTOMER', 'ADMIN'));
