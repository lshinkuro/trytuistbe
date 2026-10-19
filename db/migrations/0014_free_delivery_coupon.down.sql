UPDATE app_config
SET value = value - 'free_delivery_coupon_enabled' - 'free_delivery_coupon_weekly', updated_at = now()
WHERE key = 'order_rules';
DROP INDEX IF EXISTS orders_free_delivery_idx;
ALTER TABLE orders DROP COLUMN free_delivery_coupon, DROP COLUMN delivery_fee_waived;
