-- Akun yang hanya bisa masuk lewat Google tidak bisa dipertahankan tanpa password.
DELETE FROM users WHERE password_hash IS NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_has_login_method;
DROP INDEX IF EXISTS users_google_sub_key;
ALTER TABLE users DROP COLUMN IF EXISTS google_sub;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
