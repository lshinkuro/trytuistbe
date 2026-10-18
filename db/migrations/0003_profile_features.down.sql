DROP TABLE IF EXISTS legal_documents;
DROP TABLE IF EXISTS help_articles;
DROP TABLE IF EXISTS help_topics;
DROP TABLE IF EXISTS user_payment_methods;
ALTER TABLE users DROP COLUMN IF EXISTS default_payment_method;

DELETE FROM app_config WHERE key IN ('ewallet_providers', 'help_contacts');
UPDATE app_config SET value = '[
    {"id":"COD","name":"Bayar di Tempat","icon":"banknote","is_enabled":true},
    {"id":"QRIS","name":"QRIS","icon":"qrcode","is_enabled":true},
    {"id":"EWALLET","name":"E-Wallet","icon":"wallet.pass","is_enabled":false}
  ]' WHERE key = 'payment_methods';
UPDATE app_config SET value = '[
    {"id":"addresses","title":"Alamat Pengiriman","icon":"location.fill","tint":"blue","action":"trytuist://addresses"},
    {"id":"payment","title":"Metode Pembayaran","icon":"creditcard.fill","tint":"green","action":"trytuist://payment-methods"},
    {"id":"favorites","title":"Menu Favorit","icon":"heart.fill","tint":"pink","action":"trytuist://favorites"},
    {"id":"help","title":"Pusat Bantuan","icon":"questionmark.circle.fill","tint":"teal","action":"https://trytuist.com/bantuan"},
    {"id":"language","title":"Bahasa","icon":"globe","tint":"indigo","action":"trytuist://language"},
    {"id":"privacy","title":"Kebijakan Privasi","icon":"lock.fill","tint":"gray","action":"https://trytuist.com/privasi"}
  ]' WHERE key = 'profile_menu';
