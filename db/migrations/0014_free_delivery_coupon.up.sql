-- 0014_free_delivery_coupon: kupon gratis ongkir mingguan (API_PLAN.md §7).
-- Kuota per user per minggu (Senin 00:00 WIB). Kupon dipakai hanya bila user mengaktifkannya saat checkout;
-- pesanan batal/gagal tidak dihitung (kupon kembali).
ALTER TABLE orders
    ADD COLUMN free_delivery_coupon BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN delivery_fee_waived  BIGINT  NOT NULL DEFAULT 0 CHECK (delivery_fee_waived >= 0); -- ongkir yang ditanggung toko
CREATE INDEX orders_free_delivery_idx ON orders (user_id, created_at) WHERE free_delivery_coupon;

-- Aktif, 2 kupon per minggu. Diubah super admin di web admin → Ongkir.
UPDATE app_config
SET value = value || '{"free_delivery_coupon_enabled":true,"free_delivery_coupon_weekly":2}'::jsonb,
    updated_at = now()
WHERE key = 'order_rules';
