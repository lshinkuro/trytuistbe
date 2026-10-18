-- §5.11: checkout memakai metode pembayaran & alamat utama user.
-- Info akun e-wallet disalin ke pesanan (snapshot), jadi riwayat tetap terbaca
-- walaupun e-wallet-nya nanti dihapus user.
ALTER TABLE orders
    ADD COLUMN payment_account_id     UUID REFERENCES user_payment_methods(id) ON DELETE SET NULL,
    ADD COLUMN payment_provider       TEXT,
    ADD COLUMN payment_account_masked TEXT;
