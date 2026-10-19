-- 0015_banner_actions_combos: banner yang benar-benar bisa di-tap + menu combo (paket isi tetap).

-- Banner: aksi saat di-tap menggantikan kolom deeplink yang tidak pernah dipakai.
ALTER TABLE banners
    ADD COLUMN action_type  TEXT        NOT NULL DEFAULT 'NONE'
        CHECK (action_type IN ('NONE', 'PRODUCT', 'CATEGORY', 'FREE_DELIVERY')),
    ADD COLUMN action_value TEXT,                                  -- id produk / kategori
    ADD COLUMN updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT banners_action_value_check CHECK (
        (action_type IN ('PRODUCT', 'CATEGORY')) = (action_value IS NOT NULL AND action_value <> ''));
ALTER TABLE banners DROP COLUMN deeplink;

-- Combo: produk biasa (harga sendiri) + daftar isi. Isi tidak boleh combo lagi (dicek di API).
CREATE TABLE product_bundle_items (
    bundle_id  TEXT    NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    product_id TEXT    NOT NULL REFERENCES products(id),
    option_id  TEXT,                                               -- varian isi (mis. large); NULL = varian default
    quantity   INTEGER NOT NULL CHECK (quantity BETWEEN 1 AND 10),
    sort_order INTEGER NOT NULL DEFAULT 0,
    CHECK (bundle_id <> product_id)
);
CREATE UNIQUE INDEX product_bundle_items_unique ON product_bundle_items (bundle_id, product_id, COALESCE(option_id, ''));
CREATE INDEX product_bundle_items_product_idx ON product_bundle_items (product_id);

-- Snapshot isi paket di pesanan: dapur tahu yang disiapkan & stok isi bisa dikembalikan saat batal.
ALTER TABLE order_items ADD COLUMN bundle_items JSONB;
