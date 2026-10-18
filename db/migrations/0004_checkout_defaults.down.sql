ALTER TABLE orders
    DROP COLUMN IF EXISTS payment_account_masked,
    DROP COLUMN IF EXISTS payment_provider,
    DROP COLUMN IF EXISTS payment_account_id;
