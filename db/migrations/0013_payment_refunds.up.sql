-- 0013_payment_refunds: refund dari web admin (API_PLAN.md §5.27, fase 5b).
-- MIDTRANS = lewat Refund API; MANUAL = admin sudah transfer balik sendiri (VA / gerai tidak bisa direfund lewat API).
CREATE TABLE payment_refunds (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),  -- juga refund_key ke Midtrans (aman diulang)
    payment_id UUID        NOT NULL REFERENCES payments(id),
    amount     BIGINT      NOT NULL CHECK (amount > 0),
    reason     TEXT        NOT NULL,
    method     TEXT        NOT NULL CHECK (method IN ('MIDTRANS','MANUAL')),
    -- PENDING dihitung sebagai terpakai, jadi dua refund bersamaan tidak bisa melebihi total.
    status     TEXT        NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','SUCCEEDED','FAILED')),
    message    TEXT,                                        -- alasan gagal dari Midtrans
    admin_id   UUID        REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payment_refunds_payment_idx ON payment_refunds (payment_id, created_at);

-- Daftar Pembayaran di web admin diurutkan terbaru.
CREATE INDEX payments_created_idx ON payments (created_at DESC);
