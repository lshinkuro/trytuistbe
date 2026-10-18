CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- ========== User & Auth ==========
CREATE TABLE users (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT        NOT NULL,
    email          TEXT        NOT NULL,
    password_hash  TEXT        NOT NULL,
    phone          TEXT,
    avatar_url     TEXT,
    member_tier    TEXT        NOT NULL DEFAULT 'SILVER' CHECK (member_tier IN ('SILVER','GOLD','PLATINUM')),
    points         INTEGER     NOT NULL DEFAULT 0 CHECK (points >= 0),
    balance        BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    fcm_token      TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL UNIQUE,          -- simpan SHA-256, bukan token asli
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    user_agent  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- ========== Katalog ==========
CREATE TABLE categories (
    id          TEXT PRIMARY KEY,                     -- 'coffee', 'rice', ...
    name        TEXT    NOT NULL,
    icon        TEXT    NOT NULL DEFAULT '🍽️',
    sort_order  INTEGER NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE products (
    id              TEXT PRIMARY KEY,                 -- 'P01', ...
    category_id     TEXT          NOT NULL REFERENCES categories(id),
    name            TEXT          NOT NULL,
    description     TEXT          NOT NULL DEFAULT '',
    price           BIGINT        NOT NULL CHECK (price >= 0),   -- Rupiah
    image_url       TEXT,
    rating          NUMERIC(2,1)  NOT NULL DEFAULT 0 CHECK (rating BETWEEN 0 AND 5),
    sold_count      INTEGER       NOT NULL DEFAULT 0,
    is_available    BOOLEAN       NOT NULL DEFAULT TRUE,
    is_best_seller  BOOLEAN       NOT NULL DEFAULT FALSE,
    sort_order      INTEGER       NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX products_category_idx ON products (category_id, sort_order);

CREATE TABLE product_options (
    product_id   TEXT    NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    id           TEXT    NOT NULL,                    -- 'regular', 'large', 'spicy', ...
    name         TEXT    NOT NULL,
    extra_price  BIGINT  NOT NULL DEFAULT 0 CHECK (extra_price >= 0),
    sort_order   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, id)
);

CREATE TABLE banners (
    id          TEXT PRIMARY KEY,
    title       TEXT        NOT NULL,
    subtitle    TEXT        NOT NULL DEFAULT '',
    image_url   TEXT        NOT NULL,
    deeplink    TEXT,                                -- mis. 'trytuist://category/coffee'
    starts_at   TIMESTAMPTZ,
    ends_at     TIMESTAMPTZ,
    sort_order  INTEGER     NOT NULL DEFAULT 0,
    is_active   BOOLEAN     NOT NULL DEFAULT TRUE
);

CREATE TABLE stores (
    id        TEXT PRIMARY KEY,                      -- 'ST01'
    name      TEXT             NOT NULL,             -- 'Gandaria City'
    address   TEXT             NOT NULL,
    lat       DOUBLE PRECISION NOT NULL,
    lng       DOUBLE PRECISION NOT NULL,
    is_open   BOOLEAN          NOT NULL DEFAULT TRUE
);

-- ========== Alamat, favorit, voucher ==========
CREATE TABLE addresses (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label       TEXT    NOT NULL,                    -- 'Rumah', 'Kantor'
    recipient   TEXT    NOT NULL,
    phone       TEXT    NOT NULL,
    address     TEXT    NOT NULL,
    notes       TEXT,
    lat         DOUBLE PRECISION,
    lng         DOUBLE PRECISION,
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX addresses_one_default ON addresses (user_id) WHERE is_default;

CREATE TABLE favorites (
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    product_id  TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, product_id)
);

CREATE TABLE vouchers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code            TEXT   NOT NULL UNIQUE,
    title           TEXT   NOT NULL,
    description     TEXT   NOT NULL DEFAULT '',
    discount_type   TEXT   NOT NULL CHECK (discount_type IN ('PERCENT','FIXED')),
    discount_value  BIGINT NOT NULL CHECK (discount_value > 0),
    min_subtotal    BIGINT NOT NULL DEFAULT 0,
    max_discount    BIGINT,
    starts_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at         TIMESTAMPTZ NOT NULL,
    quota           INTEGER
);

CREATE TABLE user_vouchers (
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    voucher_id  UUID NOT NULL REFERENCES vouchers(id) ON DELETE CASCADE,
    used_at     TIMESTAMPTZ,
    PRIMARY KEY (user_id, voucher_id)
);

-- ========== Pesanan ==========
CREATE TYPE order_status AS ENUM ('PENDING','PAID','PROCESSING','DELIVERING','COMPLETED','CANCELLED','FAILED');

CREATE TABLE orders (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_number     TEXT          NOT NULL UNIQUE,      -- 'ORD-20261017-0042'
    user_id          UUID          NOT NULL REFERENCES users(id),
    store_id         TEXT          NOT NULL REFERENCES stores(id),
    address_id       UUID          REFERENCES addresses(id) ON DELETE SET NULL,
    status           order_status  NOT NULL DEFAULT 'PENDING',
    subtotal         BIGINT        NOT NULL,
    delivery_fee     BIGINT        NOT NULL DEFAULT 0,
    discount         BIGINT        NOT NULL DEFAULT 0,
    total            BIGINT        NOT NULL CHECK (total >= 0),
    voucher_id       UUID          REFERENCES vouchers(id),
    payment_method   TEXT          NOT NULL CHECK (payment_method IN ('COD','QRIS','EWALLET')),
    note             TEXT,
    idempotency_key  TEXT,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);
CREATE UNIQUE INDEX orders_idempotency_key ON orders (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE order_items (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id      UUID    NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id    TEXT    NOT NULL REFERENCES products(id),
    product_name  TEXT    NOT NULL,                  -- snapshot saat order dibuat
    option_id     TEXT,
    option_name   TEXT,
    unit_price    BIGINT  NOT NULL,                  -- snapshot harga (produk + opsi)
    quantity      INTEGER NOT NULL CHECK (quantity BETWEEN 1 AND 20),
    note          VARCHAR(100) NOT NULL DEFAULT '',
    subtotal      BIGINT  NOT NULL,
    sort_order    INTEGER NOT NULL DEFAULT 0          -- urutan item seperti di request
);
CREATE INDEX order_items_order_idx ON order_items (order_id);

CREATE TABLE order_status_history (
    order_id  UUID         NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    status    order_status NOT NULL,
    at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX order_status_history_order_idx ON order_status_history (order_id, at);

CREATE TABLE order_ratings (
    order_id    UUID PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
    rating      SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
