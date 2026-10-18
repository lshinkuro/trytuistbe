-- 0010_push_devices: device token APNs untuk notifikasi push (API_PLAN.md §5.28).
CREATE TABLE push_devices (
    token        TEXT PRIMARY KEY,                       -- device token APNs (hex)
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform     TEXT        NOT NULL DEFAULT 'ios' CHECK (platform IN ('ios')),
    bundle_id    TEXT        NOT NULL,                   -- apns-topic, mis. com.lshinkuro.try-tuist.dev
    environment  TEXT        NOT NULL CHECK (environment IN ('sandbox','production')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX push_devices_user_idx ON push_devices (user_id);
