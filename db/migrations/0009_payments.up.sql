-- 0009_payments: pembayaran online Midtrans Snap (API_PLAN.md §5.27).
-- Checkout online membuat sesi `payments` dulu; baris `orders` baru dibuat saat lunas.

CREATE TYPE payment_status AS ENUM ('PENDING','PAID','FAILED','EXPIRED','CANCELLED');

CREATE TABLE payments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID           NOT NULL REFERENCES users(id),
    store_id          TEXT           NOT NULL REFERENCES stores(id),
    provider          TEXT           NOT NULL DEFAULT 'MIDTRANS',
    provider_order_id TEXT           NOT NULL UNIQUE,          -- order_id yang dikirim ke Midtrans
    status            payment_status NOT NULL DEFAULT 'PENDING',
    amount            BIGINT         NOT NULL CHECK (amount > 0),
    snapshot          JSONB          NOT NULL,                 -- quote + catatan saat checkout
    request_hash      TEXT           NOT NULL,                 -- isi keranjang, untuk cek Idempotency-Key
    idempotency_key   TEXT,
    snap_token        TEXT,
    redirect_url      TEXT,
    payment_type      TEXT,                                    -- gopay | qris | bank_transfer | credit_card | …
    provider_txn_id   TEXT,
    provider_status   TEXT,
    status_message    TEXT,
    refund_status     TEXT,                                    -- NULL | REFUND | PARTIAL_REFUND | CHARGEBACK | PARTIAL_CHARGEBACK | REVERSED | REQUIRED
    expires_at        TIMESTAMPTZ    NOT NULL,
    paid_at           TIMESTAMPTZ,
    last_checked_at   TIMESTAMPTZ,
    order_id          UUID UNIQUE REFERENCES orders(id),
    created_at        TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ    NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payments_idempotency ON payments (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX payments_pending_idx ON payments (expires_at) WHERE status = 'PENDING';
CREATE INDEX payments_user_idx ON payments (user_id, created_at DESC);

CREATE TABLE payment_events (
    id              BIGSERIAL PRIMARY KEY,
    payment_id      UUID REFERENCES payments(id) ON DELETE CASCADE,
    source          TEXT        NOT NULL,                      -- webhook | status_api | sweeper | client
    provider_status TEXT,
    signature_ok    BOOLEAN,
    payload         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payment_events_payment_idx ON payment_events (payment_id, created_at);

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_payment_method_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_method_check
    CHECK (payment_method IN ('COD','QRIS','EWALLET','ONLINE'));
ALTER TABLE orders ADD COLUMN payment_id UUID UNIQUE REFERENCES payments(id);

-- Metode bawaan baru "Bayar Online" (paling atas). Aktif hanya bila MIDTRANS_SERVER_KEY diisi.
UPDATE app_config
SET value = '[{"id":"ONLINE","name":"Bayar Online","icon":"creditcard.fill","description":"GoPay, QRIS, ShopeePay, transfer bank (VA), kartu","is_enabled":true}]'::jsonb
            || (SELECT COALESCE(jsonb_agg(e), '[]'::jsonb) FROM jsonb_array_elements(value) e WHERE e->>'id' <> 'ONLINE'),
    updated_at = now()
WHERE key = 'payment_methods';
