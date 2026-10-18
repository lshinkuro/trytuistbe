DROP INDEX IF EXISTS orders_store_created_idx;
DROP TABLE IF EXISTS store_product_availability;
ALTER TABLE stores
    DROP COLUMN IF EXISTS created_at,
    DROP COLUMN IF EXISTS sort_order,
    DROP COLUMN IF EXISTS is_active,
    DROP COLUMN IF EXISTS delivery_radius_km,
    DROP COLUMN IF EXISTS closes_at,
    DROP COLUMN IF EXISTS opens_at,
    DROP COLUMN IF EXISTS phone;
