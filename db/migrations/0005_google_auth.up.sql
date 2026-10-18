-- §5.3b: daftar / masuk dengan Google.
-- Akun Google tidak punya password; akun email+password bisa ditautkan ke Google
-- (email yang sama, sudah diverifikasi Google).
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users ADD COLUMN google_sub TEXT;
CREATE UNIQUE INDEX users_google_sub_key ON users (google_sub) WHERE google_sub IS NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_has_login_method
    CHECK (password_hash IS NOT NULL OR google_sub IS NOT NULL);
