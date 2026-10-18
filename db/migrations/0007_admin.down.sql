DROP INDEX IF EXISTS orders_status_created_idx;
DROP INDEX IF EXISTS orders_created_idx;
DROP TABLE IF EXISTS admin_audit_logs;
ALTER TABLE order_status_history DROP COLUMN IF EXISTS changed_by, DROP COLUMN IF EXISTS note;
ALTER TABLE users DROP COLUMN IF EXISTS role;
