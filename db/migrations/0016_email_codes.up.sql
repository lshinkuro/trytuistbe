-- 0016_email_codes: verifikasi email & reset password dengan kode 6 digit (dikirim lewat Resend).
ALTER TABLE users ADD COLUMN email_verified_at TIMESTAMPTZ;
-- Akun yang sudah ada dianggap terverifikasi (fitur baru; mereka sudah memakai email itu).
UPDATE users SET email_verified_at = created_at;

CREATE TABLE email_codes (
    id          BIGSERIAL PRIMARY KEY,
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose     TEXT        NOT NULL CHECK (purpose IN ('VERIFY_EMAIL', 'RESET_PASSWORD')),
    code_hash   TEXT        NOT NULL,                   -- SHA-256, kode asli tidak disimpan
    attempts    INTEGER     NOT NULL DEFAULT 0,         -- salah > 5 kali → kode hangus
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,                            -- dipakai / diganti kode yang lebih baru
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX email_codes_user_idx ON email_codes (user_id, purpose, created_at DESC);
