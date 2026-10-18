-- §5.25: web admin (trytuistadmin). Login memakai POST /v1/auth/login yang sama;
-- hanya user dengan role ADMIN yang boleh memanggil /v1/admin/*.
ALTER TABLE users
    ADD COLUMN role TEXT NOT NULL DEFAULT 'CUSTOMER' CHECK (role IN ('CUSTOMER', 'ADMIN'));

-- Siapa & kenapa status pesanan diubah (mis. "Pembayaran ditolak bank").
ALTER TABLE order_status_history
    ADD COLUMN note       TEXT,
    ADD COLUMN changed_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- Jejak semua perubahan dari web admin.
CREATE TABLE admin_audit_logs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id    UUID        REFERENCES users(id) ON DELETE SET NULL,
    action      TEXT        NOT NULL,          -- ORDER_STATUS | PRODUCT_CREATE | PRODUCT_UPDATE | DEAL_CREATE | DEAL_UPDATE | DEAL_DELETE
    entity      TEXT        NOT NULL,          -- order | product | deal
    entity_id   TEXT        NOT NULL,
    summary     TEXT        NOT NULL,          -- kalimat singkat untuk ditampilkan
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admin_audit_logs_created_idx ON admin_audit_logs (created_at DESC);

-- Query dashboard & daftar pesanan admin (semua user, rentang tanggal).
CREATE INDEX orders_created_idx ON orders (created_at DESC);
CREATE INDEX orders_status_created_idx ON orders (status, created_at DESC);
