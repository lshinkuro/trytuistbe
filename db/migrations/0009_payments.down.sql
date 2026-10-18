UPDATE app_config
SET value = (SELECT COALESCE(jsonb_agg(e), '[]'::jsonb) FROM jsonb_array_elements(value) e WHERE e->>'id' <> 'ONLINE'),
    updated_at = now()
WHERE key = 'payment_methods';

UPDATE users SET default_payment_method = 'COD' WHERE default_payment_method = 'ONLINE';

ALTER TABLE orders DROP COLUMN IF EXISTS payment_id;
UPDATE orders SET payment_method = 'QRIS' WHERE payment_method = 'ONLINE';
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_payment_method_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_method_check
    CHECK (payment_method IN ('COD','QRIS','EWALLET'));

DROP TABLE IF EXISTS payment_events;
DROP TABLE IF EXISTS payments;
DROP TYPE IF EXISTS payment_status;
