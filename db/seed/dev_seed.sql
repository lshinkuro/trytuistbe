-- Data contoh untuk dev/staging. Sama dengan mock iOS (CatalogFixtures.swift).
-- Dijalankan oleh `go run ./cmd/seed` (aman dijalankan berulang kali).
-- Akun demo (kholis@gmail.com / 12345678, demo@trytuist.com / password123) dibuat
-- oleh cmd/seed karena password_hash perlu bcrypt.

-- Kategori
INSERT INTO categories (id, name, icon, sort_order) VALUES
  ('coffee', 'Kopi', '☕️', 1),
  ('non_coffee', 'Non-Kopi', '🍵', 2),
  ('rice', 'Nasi & Mie', '🍛', 3),
  ('fast_food', 'Burger & Ayam', '🍔', 4),
  ('snack', 'Camilan', '🍟', 5),
  ('dessert', 'Dessert', '🍰', 6)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, icon = EXCLUDED.icon, sort_order = EXCLUDED.sort_order;

-- Banner promo
INSERT INTO banners (id, title, subtitle, image_url, sort_order) VALUES
  ('B1', 'Diskon 30% Makan Siang', 'Semua menu makanan, khusus jam 11–14', 'https://images.unsplash.com/photo-1547573854-74d2a71d0826?w=1200&q=80&auto=format&fit=crop', 1),
  ('B2', 'Gratis Ongkir Rp 0', 'Min. belanja Rp 50.000 se-Jakarta', 'https://images.unsplash.com/photo-1424847651672-bf20a4b0982b?w=1200&q=80&auto=format&fit=crop', 2),
  ('B3', 'Kopi Pagi Cuma 15rb', 'Setiap hari sebelum jam 10 pagi', 'https://images.unsplash.com/photo-1596018589878-217d8603c4c6?w=1200&q=80&auto=format&fit=crop', 3)
ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, subtitle = EXCLUDED.subtitle, image_url = EXCLUDED.image_url, sort_order = EXCLUDED.sort_order;

-- Produk (foto: Unsplash, lisensi gratis)
INSERT INTO products (id, category_id, name, description, price, image_url, rating, sold_count, is_available, is_best_seller, sort_order) VALUES
  ('P01', 'coffee', 'Es Kopi Susu Gula Aren', 'Espresso, susu segar, dan gula aren asli. Manisnya pas, cocok buat semangat seharian.', 22000, 'https://images.unsplash.com/photo-1767471716700-2b3aa77b1ab6?w=800&q=80&auto=format&fit=crop', 4.9, 1520, TRUE, TRUE, 1),
  ('P02', 'coffee', 'Cappuccino', 'Espresso dengan foam susu yang lembut dan tebal.', 25000, 'https://images.unsplash.com/photo-1563311977-d285756282dc?w=800&q=80&auto=format&fit=crop', 4.7, 640, TRUE, FALSE, 2),
  ('P03', 'coffee', 'Caffe Latte', 'Espresso dan susu steamed dengan latte art cantik.', 24000, 'https://images.unsplash.com/photo-1593443320739-77f74939d0da?w=800&q=80&auto=format&fit=crop', 4.6, 580, TRUE, FALSE, 3),
  ('P04', 'coffee', 'Iced Americano', 'Double shot espresso dengan air dingin dan es batu. Segar tanpa gula.', 20000, 'https://images.unsplash.com/photo-1676506739319-70bff65bfc48?w=800&q=80&auto=format&fit=crop', 4.5, 410, TRUE, FALSE, 4),
  ('P05', 'non_coffee', 'Iced Matcha Latte', 'Matcha premium dari Uji, Jepang, dengan susu segar dan es.', 27000, 'https://images.unsplash.com/photo-1749280447307-31a68eb38673?w=800&q=80&auto=format&fit=crop', 4.8, 890, TRUE, TRUE, 5),
  ('P06', 'non_coffee', 'Hot Matcha', 'Matcha hangat yang creamy, pas untuk sore hari.', 25000, 'https://images.unsplash.com/photo-1515823064-d6e0c04616a7?w=800&q=80&auto=format&fit=crop', 4.4, 220, FALSE, FALSE, 6),
  ('P07', 'rice', 'Nasi Goreng Spesial', 'Nasi goreng bumbu rumahan dengan telur, ayam suwir, dan kerupuk.', 32000, 'https://images.unsplash.com/photo-1512058564366-18510be2db19?w=800&q=80&auto=format&fit=crop', 4.8, 1210, TRUE, TRUE, 7),
  ('P08', 'rice', 'Nasi Goreng Udang', 'Nasi goreng dengan udang segar dan sayuran.', 38000, 'https://images.unsplash.com/photo-1551326844-4df70f78d0e9?w=800&q=80&auto=format&fit=crop', 4.6, 530, TRUE, FALSE, 8),
  ('P09', 'rice', 'Ramen Pedas', 'Kuah kaldu kaya rasa, mie kenyal, telur ajitama, dan chashu.', 45000, 'https://images.unsplash.com/photo-1509680859026-7d8cfc6894f4?w=800&q=80&auto=format&fit=crop', 4.7, 760, TRUE, FALSE, 9),
  ('P10', 'fast_food', 'Ayam Goreng Krispi', 'Ayam goreng tepung renyah dengan saus sambal spesial.', 29000, 'https://images.unsplash.com/photo-1585703900468-13c7a978ad86?w=800&q=80&auto=format&fit=crop', 4.6, 980, TRUE, FALSE, 10),
  ('P11', 'fast_food', 'Classic Beef Burger', 'Patty daging sapi, keju cheddar, selada, dan saus rahasia.', 42000, 'https://images.unsplash.com/photo-1543339469-94ba2391431f?w=800&q=80&auto=format&fit=crop', 4.8, 1040, TRUE, TRUE, 11),
  ('P12', 'snack', 'Kentang Goreng', 'Kentang goreng renyah dengan saus tomat.', 18000, 'https://images.unsplash.com/photo-1716042998725-0e0b957b9d99?w=800&q=80&auto=format&fit=crop', 4.5, 870, TRUE, FALSE, 12),
  ('P13', 'snack', 'Butter Croissant', 'Croissant mentega yang flaky, dipanggang setiap pagi.', 21000, 'https://images.unsplash.com/photo-1623334044303-241021148842?w=800&q=80&auto=format&fit=crop', 4.6, 450, TRUE, FALSE, 13),
  ('P14', 'dessert', 'Chocolate Cake', 'Kue cokelat lembap dengan ganache dark chocolate.', 35000, 'https://images.unsplash.com/photo-1517427294546-5aa121f68e8a?w=800&q=80&auto=format&fit=crop', 4.9, 690, TRUE, TRUE, 14)
ON CONFLICT (id) DO UPDATE SET category_id = EXCLUDED.category_id, name = EXCLUDED.name, description = EXCLUDED.description, price = EXCLUDED.price,
  image_url = EXCLUDED.image_url, is_available = EXCLUDED.is_available, is_best_seller = EXCLUDED.is_best_seller, sort_order = EXCLUDED.sort_order;

-- Varian produk
INSERT INTO product_options (product_id, id, name, extra_price, sort_order) VALUES
  ('P01', 'regular', 'Regular', 0, 1),
  ('P01', 'large', 'Large', 5000, 2),
  ('P02', 'regular', 'Regular', 0, 1),
  ('P02', 'large', 'Large', 5000, 2),
  ('P03', 'regular', 'Regular', 0, 1),
  ('P03', 'large', 'Large', 5000, 2),
  ('P04', 'regular', 'Regular', 0, 1),
  ('P04', 'large', 'Large', 5000, 2),
  ('P05', 'regular', 'Regular', 0, 1),
  ('P05', 'large', 'Large', 5000, 2),
  ('P06', 'regular', 'Regular', 0, 1),
  ('P06', 'large', 'Large', 5000, 2),
  ('P07', 'normal', 'Normal', 0, 1),
  ('P07', 'spicy', 'Pedas', 0, 2),
  ('P07', 'jumbo', 'Porsi Jumbo', 8000, 3),
  ('P08', 'normal', 'Normal', 0, 1),
  ('P08', 'spicy', 'Pedas', 0, 2),
  ('P08', 'jumbo', 'Porsi Jumbo', 8000, 3),
  ('P09', 'normal', 'Normal', 0, 1),
  ('P09', 'spicy', 'Pedas', 0, 2),
  ('P09', 'jumbo', 'Porsi Jumbo', 8000, 3),
  ('P10', 'normal', 'Normal', 0, 1),
  ('P10', 'spicy', 'Pedas', 0, 2),
  ('P10', 'jumbo', 'Porsi Jumbo', 8000, 3)
ON CONFLICT (product_id, id) DO UPDATE SET name = EXCLUDED.name, extra_price = EXCLUDED.extra_price, sort_order = EXCLUDED.sort_order;

-- Promo harga produk (§5.24). D01 tampil sebagai popup di Beranda; image_url NULL = foto produk.
INSERT INTO product_deals (id, product_id, title, subtitle, badge, image_url, deal_price, starts_at, ends_at, show_as_popup, sort_order) VALUES
  ('D01', 'P07', 'Nasi Goreng Spesial cuma 19rb!', 'Promo makan hemat minggu ini. Selama persediaan ada.', '', NULL, 19900, now() - interval '1 day', now() + interval '30 days', TRUE, 1),
  ('D02', 'P05', 'Iced Matcha Latte Hemat', 'Segarnya matcha premium, harga lebih ringan.', '', NULL, 17900, now() - interval '1 day', now() + interval '30 days', FALSE, 2),
  ('D03', 'P12', 'Kentang Goreng 12rb', 'Teman ngopi paling pas.', '', NULL, 12000, now() - interval '1 day', now() + interval '30 days', FALSE, 3)
ON CONFLICT (id) DO UPDATE SET product_id = EXCLUDED.product_id, title = EXCLUDED.title, subtitle = EXCLUDED.subtitle,
  badge = EXCLUDED.badge, image_url = EXCLUDED.image_url, deal_price = EXCLUDED.deal_price, starts_at = EXCLUDED.starts_at,
  ends_at = EXCLUDED.ends_at, show_as_popup = EXCLUDED.show_as_popup, sort_order = EXCLUDED.sort_order, is_active = TRUE;

-- Outlet
-- Outlet (§5.26). ST01 & ST02 buka 24 jam (00:00–00:00) supaya contoh & test bisa dipakai kapan saja.
INSERT INTO stores (id, name, address, lat, lng, phone, opens_at, closes_at, delivery_radius_km, sort_order) VALUES
  ('ST01', 'Gandaria City', 'Jl. Sultan Iskandar Muda, Jakarta Selatan', -6.2443, 106.7834, '021-2900-1001', '00:00', '00:00', 7, 1),
  ('ST02', 'Kemang Utara', 'Jl. Kemang Utara Raya, Jakarta Selatan', -6.2608, 106.8133, '021-2900-1002', '00:00', '00:00', 5, 2),
  ('ST03', 'Kelapa Gading', 'Jl. Boulevard Raya, Kelapa Gading, Jakarta Utara', -6.1588, 106.9057, '021-2900-1003', '10:00', '22:00', 6, 3)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, address = EXCLUDED.address, lat = EXCLUDED.lat, lng = EXCLUDED.lng,
  phone = EXCLUDED.phone, opens_at = EXCLUDED.opens_at, closes_at = EXCLUDED.closes_at,
  delivery_radius_km = EXCLUDED.delivery_radius_km, sort_order = EXCLUDED.sort_order;

-- Stok per outlet: Ramen Pedas habis di Kemang, Chocolate Cake habis di Kelapa Gading.
INSERT INTO store_product_availability (store_id, product_id, is_available) VALUES
  ('ST02', 'P09', FALSE),
  ('ST03', 'P14', FALSE)
ON CONFLICT (store_id, product_id) DO UPDATE SET is_available = EXCLUDED.is_available;

-- Voucher
INSERT INTO vouchers (code, title, description, discount_type, discount_value, min_subtotal, max_discount, starts_at, ends_at) VALUES
  ('HEMAT10', 'Hemat 10%', 'Diskon 10% untuk semua menu, maks. Rp 20.000.', 'PERCENT', 10, 30000, 20000, now() - interval '1 day', now() + interval '365 days'),
  ('DISKON15K', 'Potongan Rp 15.000', 'Min. belanja Rp 75.000.', 'FIXED', 15000, 75000, NULL, now() - interval '1 day', now() + interval '365 days'),
  ('KOPIPAGI', 'Kopi Pagi Hemat', 'Potongan Rp 7.000 untuk pesanan kopi pagi.', 'FIXED', 7000, 20000, NULL, now() - interval '1 day', now() + interval '365 days')
ON CONFLICT (code) DO UPDATE SET title = EXCLUDED.title, description = EXCLUDED.description, discount_type = EXCLUDED.discount_type,
  discount_value = EXCLUDED.discount_value, min_subtotal = EXCLUDED.min_subtotal, max_discount = EXCLUDED.max_discount, ends_at = EXCLUDED.ends_at;
