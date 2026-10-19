-- 0012_delivery_distance: ongkir per jarak & estimasi waktu antar (API_PLAN.md §7).
-- Snapshot di pesanan supaya riwayat tetap sama walau aturan ongkir diubah.
ALTER TABLE orders
    ADD COLUMN distance_km     NUMERIC(5,1),   -- NULL = alamat tanpa koordinat
    ADD COLUMN eta_min_minutes INTEGER,
    ADD COLUMN eta_max_minutes INTEGER;

-- Rp 10.000 sampai 3 km, + Rp 2.000 per km berikutnya, maks. Rp 30.000.
-- ETA = 15 menit siapkan + perjalanan 20 km/jam. Ubah lewat UPDATE app_config tanpa deploy.
UPDATE app_config
SET value = value || '{"delivery_base_km":3,"delivery_fee_per_km":2000,"delivery_fee_max":30000,"prep_minutes":15,"courier_speed_kmh":20}'::jsonb,
    updated_at = now()
WHERE key = 'order_rules';
