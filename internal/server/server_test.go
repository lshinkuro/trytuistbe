package server_test

// Integration test: menjalankan router asli terhadap database Postgres sungguhan.
// Butuh TEST_DATABASE_URL (database KHUSUS test — isinya dihapus setiap run), mis.:
//
//	TEST_DATABASE_URL=postgres://trytuist:trytuist@localhost:5432/trytuist_test?sslmode=disable go test ./...

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"trytuist-api/db"
	"trytuist-api/internal/auth"
	"trytuist-api/internal/config"
	"trytuist-api/internal/database"
	"trytuist-api/internal/server"
)

const apiKey = "test-api-key"

type env struct {
	t    *testing.T
	srv  *httptest.Server
	pool *pgxpool.Pool
}

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL belum di-set; integration test dilewati")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	resetDB(t, pool)

	auth.BcryptCost = bcrypt.MinCost
	cfg := config.Config{
		Env: "dev", APIKeys: []string{apiKey}, JWTSecret: "test-secret-test-secret-test-secret",
		JWTAccessTTL: 15 * time.Minute, JWTRefreshTTL: time.Hour, Timezone: time.FixedZone("WIB", 7*3600),
		DeliveryFee: 10000, LoginRateLimit: 100,
	}
	srv := httptest.NewServer(server.NewRouter(cfg, pool))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, pool: pool}
}

func resetDB(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.NewMigrator(pool, db.Migrations, "migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Seed(ctx, pool, db.Seed, "seed", bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
}

type resp struct {
	Status int
	Body   map[string]any
}

func (r resp) data() map[string]any { m, _ := r.Body["data"].(map[string]any); return m }
func (r resp) list() []any          { l, _ := r.Body["data"].([]any); return l }
func (r resp) errType() string {
	e, _ := r.Body["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

func (e *env) do(method, path, token string, body any, headers ...string) resp {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			e.t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return resp{Status: res.StatusCode, Body: out}
}

func (e *env) login(email, password string) (access, refresh string) {
	e.t.Helper()
	r := e.do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": password})
	if r.Status != 200 {
		e.t.Fatalf("login gagal: %d %v", r.Status, r.Body)
	}
	return r.data()["access_token"].(string), r.data()["refresh_token"].(string)
}

func expect(t *testing.T, r resp, status int, errType string) {
	t.Helper()
	if r.Status != status || r.errType() != errType {
		t.Fatalf("dapat %d %q, harap %d %q: %v", r.Status, r.errType(), status, errType, r.Body)
	}
}

func TestAuthFlow(t *testing.T) {
	e := setup(t)

	expect(t, e.do("POST", "/v1/auth/login", "", map[string]any{"email": "kholis@gmail.com", "password": "salah"}), 401, "INVALID_CREDENTIALS")
	expect(t, e.do("POST", "/v1/auth/login", "", map[string]any{"email": "tidakada@mail.com", "password": "12345678"}), 401, "INVALID_CREDENTIALS")

	access, refresh := e.login("KHOLIS@gmail.com", "12345678") // email tidak case-sensitive
	expect(t, e.do("GET", "/v1/me", access, nil), 200, "")

	// Rotation: refresh lama dicabut, yang baru dipakai.
	r := e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	expect(t, r, 200, "")
	refresh2 := r.data()["refresh_token"].(string)
	if refresh2 == refresh {
		t.Fatal("refresh token harus diganti")
	}
	// Reuse detection: token lama dipakai lagi → semua sesi dicabut.
	expect(t, e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh}), 401, "UNAUTHORIZED")
	expect(t, e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh2}), 401, "UNAUTHORIZED")

	// Register
	r = e.do("POST", "/v1/auth/register", "", map[string]any{"email": "baru@mail.com", "password": "rahasia123"})
	expect(t, r, 201, "")
	if r.data()["user_name"] != "baru" {
		t.Fatalf("nama default harus bagian depan email, dapat %v", r.data()["user_name"])
	}
	expect(t, e.do("POST", "/v1/auth/register", "", map[string]any{"email": "Baru@Mail.com", "password": "rahasia123"}), 409, "EMAIL_ALREADY_REGISTERED")
	expect(t, e.do("POST", "/v1/auth/register", "", map[string]any{"email": "x", "password": "123"}), 400, "VALIDATION_ERROR")

	// Logout mencabut refresh token.
	a, rt := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("POST", "/v1/auth/logout", a, map[string]any{"refresh_token": rt}), 200, "")
	expect(t, e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": rt}), 401, "UNAUTHORIZED")

	// Tanpa API key / token
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/categories", nil)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 403 {
		t.Fatalf("tanpa API key harus 403, dapat %d", res.StatusCode)
	}
	expect(t, e.do("GET", "/v1/me", "", nil), 401, "UNAUTHORIZED")
}

func TestCatalog(t *testing.T) {
	e := setup(t)

	if n := len(e.do("GET", "/v1/banners", "", nil).list()); n != 3 {
		t.Fatalf("banner = %d, harap 3", n)
	}
	if n := len(e.do("GET", "/v1/categories", "", nil).list()); n != 6 {
		t.Fatalf("kategori = %d, harap 6", n)
	}
	r := e.do("GET", "/v1/products?limit=100", "", nil)
	if n := len(r.list()); n != 14 {
		t.Fatalf("produk = %d, harap 14", n)
	}
	if meta := r.Body["meta"].(map[string]any); meta["total"].(float64) != 14 {
		t.Fatalf("meta.total = %v", meta["total"])
	}
	if n := len(e.do("GET", "/v1/products?category_id=coffee", "", nil).list()); n != 4 {
		t.Fatalf("produk kopi = %d, harap 4", n)
	}
	if n := len(e.do("GET", "/v1/products?best_seller=true", "", nil).list()); n != 5 {
		t.Fatalf("best seller = %d, harap 5", n)
	}
	first := e.do("GET", "/v1/products?sort=price_desc&limit=1", "", nil).list()[0].(map[string]any)
	if first["product_id"] != "P09" {
		t.Fatalf("produk termahal harus P09, dapat %v", first["product_id"])
	}
	expect(t, e.do("GET", "/v1/products?sort=random", "", nil), 400, "VALIDATION_ERROR")
	expect(t, e.do("GET", "/v1/products?limit=101", "", nil), 400, "VALIDATION_ERROR")

	p := e.do("GET", "/v1/products/P06", "", nil).data()
	if p["is_available"] != false {
		t.Fatal("P06 harus habis")
	}
	expect(t, e.do("GET", "/v1/products/P99", "", nil), 404, "NOT_FOUND")
}

func TestOrderFlow(t *testing.T) {
	e := setup(t)
	token, _ := e.login("kholis@gmail.com", "12345678")

	body := map[string]any{
		"store_id": "ST01", "voucher_code": "HEMAT10", "payment_method": "COD",
		"items": []map[string]any{
			{"product_id": "P01", "option_id": "large", "quantity": 2, "note": "less sugar"},
			{"product_id": "P11", "option_id": nil, "quantity": 1, "note": ""},
		},
	}

	// Quote = contoh di API_PLAN.md §5.10
	q := e.do("POST", "/v1/orders/quote", token, body)
	expect(t, q, 200, "")
	for k, want := range map[string]float64{"subtotal": 96000, "delivery_fee": 10000, "discount": 9600, "total": 96400} {
		if got := q.data()[k].(float64); got != want {
			t.Errorf("%s = %v, harap %v", k, got, want)
		}
	}

	// Checkout + idempotency
	r := e.do("POST", "/v1/orders", token, body, "Idempotency-Key", "key-1")
	expect(t, r, 201, "")
	orderID := r.data()["order_id"].(string)
	if !strings.HasPrefix(r.data()["order_number"].(string), "ORD-") || r.data()["status"] != "PENDING" {
		t.Fatalf("order tidak sesuai: %v", r.data())
	}
	again := e.do("POST", "/v1/orders", token, body, "Idempotency-Key", "key-1")
	if again.data()["order_id"] != orderID {
		t.Fatal("Idempotency-Key sama harus mengembalikan order yang sama")
	}
	// Voucher sekali pakai
	expect(t, e.do("POST", "/v1/orders", token, body), 422, "INVALID_VOUCHER")

	// Riwayat
	list := e.do("GET", "/v1/orders", token, nil)
	if len(list.list()) != 1 {
		t.Fatalf("harap 1 order, dapat %d", len(list.list()))
	}
	sum := list.list()[0].(map[string]any)
	if sum["product_name"] != "Es Kopi Susu Gula Aren +1 lainnya" || sum["qty"].(float64) != 3 || sum["store_name"] != "Gandaria City" {
		t.Fatalf("ringkasan order salah: %v", sum)
	}

	// Batal → voucher bisa dipakai lagi
	expect(t, e.do("POST", "/v1/orders/"+orderID+"/rating", token, map[string]any{"rating": 5}), 422, "ORDER_NOT_RATABLE")
	c := e.do("POST", "/v1/orders/"+orderID+"/cancel", token, nil)
	expect(t, c, 200, "")
	if c.data()["status"] != "CANCELLED" {
		t.Fatalf("status = %v", c.data()["status"])
	}
	expect(t, e.do("POST", "/v1/orders/"+orderID+"/cancel", token, nil), 422, "ORDER_NOT_CANCELLABLE")
	if d := e.do("POST", "/v1/orders/quote", token, body).data()["discount"].(float64); d != 9600 {
		t.Fatal("voucher harus kembali setelah pesanan batal")
	}

	// Selesai → beri nilai
	r = e.do("POST", "/v1/orders", token, map[string]any{"payment_method": "QRIS",
		"items": []map[string]any{{"product_id": "P14", "quantity": 1}}})
	expect(t, r, 201, "")
	id2 := r.data()["order_id"].(string)
	for _, s := range []string{"PAID", "PROCESSING", "DELIVERING", "COMPLETED"} {
		expect(t, e.do("POST", "/v1/dev/orders/"+id2+"/status", token, map[string]any{"status": s}), 200, "")
	}
	expect(t, e.do("POST", "/v1/orders/"+id2+"/rating", token, map[string]any{"rating": 4, "comment": "Enak"}), 200, "")
	expect(t, e.do("POST", "/v1/orders/"+id2+"/rating", token, map[string]any{"rating": 4}), 409, "ALREADY_RATED")
	if rating := e.do("GET", "/v1/products/P14", "", nil).data()["rating"].(float64); rating != 4 {
		t.Fatalf("rating produk = %v, harap 4", rating)
	}

	// Order milik user lain tidak terlihat
	other, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("GET", "/v1/orders/"+id2, other, nil), 404, "NOT_FOUND")

	// Aturan bisnis
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"items": []any{}}), 422, "EMPTY_CART")
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"items": []map[string]any{{"product_id": "P06", "quantity": 1}}}), 422, "PRODUCT_UNAVAILABLE")
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"items": []map[string]any{{"product_id": "P01", "quantity": 21}}}), 422, "INVALID_QUANTITY")
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"payment_method": "CASH", "items": []map[string]any{{"product_id": "P01", "quantity": 1}}}), 400, "VALIDATION_ERROR")
}

func TestProfile(t *testing.T) {
	e := setup(t)
	token, _ := e.login("kholis@gmail.com", "12345678")

	me := e.do("GET", "/v1/me", token, nil).data()
	if me["member_tier"] != "GOLD" || me["voucher_count"].(float64) != 3 {
		t.Fatalf("profil: %v", me)
	}
	if e.do("PATCH", "/v1/me", token, map[string]any{"name": "Kholis A"}).data()["name"] != "Kholis A" {
		t.Fatal("PATCH /me gagal")
	}

	// Alamat: yang baru dijadikan default menggeser default lama.
	a := e.do("POST", "/v1/me/addresses", token, map[string]any{
		"label": "Kantor", "recipient": "Kholis", "phone": "0812", "address": "Jl. Sudirman 1", "is_default": true})
	expect(t, a, 201, "")
	addrs := e.do("GET", "/v1/me/addresses", token, nil).list()
	defaults := 0
	for _, x := range addrs {
		if x.(map[string]any)["is_default"] == true {
			defaults++
		}
	}
	if len(addrs) != 2 || defaults != 1 {
		t.Fatalf("alamat = %d, default = %d", len(addrs), defaults)
	}
	expect(t, e.do("DELETE", "/v1/me/addresses/"+a.data()["id"].(string), token, nil), 200, "")

	// Favorit idempotent
	expect(t, e.do("PUT", "/v1/me/favorites/P05", token, nil), 200, "")
	expect(t, e.do("PUT", "/v1/me/favorites/P05", token, nil), 200, "")
	if n := len(e.do("GET", "/v1/me/favorites", token, nil).list()); n != 1 {
		t.Fatalf("favorit = %d", n)
	}
	expect(t, e.do("PUT", "/v1/me/favorites/P99", token, nil), 404, "NOT_FOUND")

	if n := len(e.do("GET", "/v1/me/vouchers", token, nil).list()); n != 3 {
		t.Fatalf("voucher = %d", n)
	}
	stores := e.do("GET", "/v1/stores?lat=-6.26&lng=106.81", "", nil).list()
	if stores[0].(map[string]any)["id"] != "ST02" {
		t.Fatal("outlet terdekat harus ST02")
	}
}

func TestHomeConfigPromo(t *testing.T) {
	e := setup(t)
	token, _ := e.login("kholis@gmail.com", "12345678")

	// Beranda tanpa token: berhasil, bagian personal null.
	guest := e.do("GET", "/v1/home", "", nil)
	expect(t, guest, 200, "")
	if guest.data()["user"] != nil || guest.data()["delivery_address"] != nil {
		t.Fatalf("tamu tidak boleh punya user/alamat: %v", guest.data())
	}
	sections := guest.data()["sections"].([]any)
	wantOrder := []string{"promo", "categories", "best_seller", "recommended"}
	if len(sections) != len(wantOrder) {
		t.Fatalf("section = %d, harap %d", len(sections), len(wantOrder))
	}
	for i, id := range wantOrder {
		if sections[i].(map[string]any)["id"] != id {
			t.Fatalf("urutan section ke-%d = %v, harap %s", i, sections[i].(map[string]any)["id"], id)
		}
	}
	best := sections[2].(map[string]any)["products"].([]any)
	if len(best) != 6 || best[0].(map[string]any)["product_id"] != "P01" {
		t.Fatalf("Paling Laris salah: %v", best)
	}
	for _, p := range best {
		if p.(map[string]any)["is_available"] != true {
			t.Fatal("produk habis tidak boleh masuk Beranda")
		}
	}

	// Dengan token: nama, poin, alamat default; outlet terdekat dari lat/lng.
	h := e.do("GET", "/v1/home?lat=-6.26&lng=106.81", token, nil).data()
	if h["user"].(map[string]any)["name"] != "Kholis" || h["delivery_address"].(map[string]any)["label"] != "Rumah" {
		t.Fatalf("data personal Beranda salah: %v", h)
	}
	if h["store"].(map[string]any)["id"] != "ST02" {
		t.Fatalf("outlet terdekat harus ST02, dapat %v", h["store"])
	}
	expect(t, e.do("GET", "/v1/home", "token-rusak", nil), 401, "UNAUTHORIZED")

	// App config + ETag
	cfg := e.do("GET", "/v1/app-config", "", nil).data()
	for _, k := range []string{"order_rules", "payment_methods", "onboarding", "profile_menu", "search_placeholder", "min_supported_version"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("app-config tidak punya %q", k)
		}
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/app-config", nil)
	req.Header.Set("X-API-Key", apiKey)
	res, _ := http.DefaultClient.Do(req)
	etag := res.Header.Get("ETag")
	res.Body.Close()
	req.Header.Set("If-None-Match", etag)
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if etag == "" || res.StatusCode != http.StatusNotModified {
		t.Fatalf("ETag %q → status %d, harap 304", etag, res.StatusCode)
	}

	// Metode bayar yang dimatikan di app_config ditolak (sejak migrasi 0003 e-wallet aktif,
	// jadi dimatikan dulu khusus untuk test ini).
	if _, err := e.pool.Exec(context.Background(), `UPDATE app_config
		SET value = jsonb_set(value, '{2,is_enabled}', 'false') WHERE key = 'payment_methods'`); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"payment_method": "EWALLET",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}}), 422, "PAYMENT_METHOD_UNAVAILABLE")
	if pm := e.do("GET", "/v1/me/payment-methods", token, nil).data(); pm["can_add_ewallet"] != false {
		t.Fatalf("e-wallet mati → can_add_ewallet harus false: %v", pm)
	}
	expect(t, e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "DANA", "account_number": "081234567890"}),
		422, "PAYMENT_METHOD_UNAVAILABLE")

	// Promo langganan
	p := e.do("GET", "/v1/promotions/featured", "", nil).data()
	if p["plan_id"] != "daily-coffee" || p["is_active"] != false {
		t.Fatalf("promo: %v", p)
	}
	a := e.do("POST", "/v1/subscriptions/daily-coffee/activate", token, nil)
	expect(t, a, 201, "")
	if a.data()["is_active"] != true || a.data()["active_until"] == nil {
		t.Fatalf("aktivasi: %v", a.data())
	}
	expect(t, e.do("POST", "/v1/subscriptions/daily-coffee/activate", token, nil), 409, "ALREADY_SUBSCRIBED")
	expect(t, e.do("POST", "/v1/subscriptions/nope/activate", token, nil), 404, "NOT_FOUND")
	expect(t, e.do("POST", "/v1/subscriptions/daily-coffee/activate", "", nil), 401, "UNAUTHORIZED")
	if e.do("GET", "/v1/promotions/featured", token, nil).data()["is_active"] != true {
		t.Fatal("promo harus aktif untuk user yang berlangganan")
	}
}

func TestProfileFeatures(t *testing.T) {
	e := setup(t)
	token, _ := e.login("kholis@gmail.com", "12345678")

	// ===== Edit profil =====
	expect(t, e.do("PATCH", "/v1/me", token, map[string]any{"phone": "12345"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/me", token, map[string]any{"name": "   "}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/me", token, map[string]any{"avatar_url": "http://x.com/a.png"}), 400, "VALIDATION_ERROR")
	me := e.do("PATCH", "/v1/me", token, map[string]any{"name": "Kholis Baru", "phone": "0812-3456-7890"})
	expect(t, me, 200, "")
	if me.data()["name"] != "Kholis Baru" || me.data()["phone"] != "081234567890" {
		t.Fatalf("PATCH /me: %v", me.data())
	}
	if e.do("PATCH", "/v1/me", token, map[string]any{"phone": ""}).data()["phone"] != nil {
		t.Fatal("phone kosong harus jadi null")
	}

	// ===== Metode pembayaran =====
	pm := e.do("GET", "/v1/me/payment-methods", token, nil)
	expect(t, pm, 200, "")
	if pm.data()["default_id"] != "COD" || len(pm.data()["methods"].([]any)) != 2 || pm.data()["can_add_ewallet"] != true {
		t.Fatalf("payment awal: %v", pm.data())
	}
	if n := len(pm.data()["ewallet_providers"].([]any)); n != 4 {
		t.Fatalf("provider = %d", n)
	}
	expect(t, e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "LINKAJA", "account_number": "081234567890"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "GOPAY", "account_number": "0212345"}), 400, "VALIDATION_ERROR")

	added := e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "gopay", "account_number": "0812 3456 7890", "set_default": true})
	expect(t, added, 201, "")
	methods := added.data()["methods"].([]any)
	wallet := methods[len(methods)-1].(map[string]any)
	walletID := wallet["id"].(string)
	if wallet["account_masked"] != "0812••••7890" || wallet["name"] != "GoPay" || added.data()["default_id"] != walletID || wallet["is_default"] != true {
		t.Fatalf("e-wallet: %v / default %v", wallet, added.data()["default_id"])
	}
	expect(t, e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "GOPAY", "account_number": "081234567890"}), 409, "PAYMENT_METHOD_EXISTS")

	if e.do("PUT", "/v1/me/payment-methods/default", token, map[string]any{"id": "qris"}).data()["default_id"] != "QRIS" {
		t.Fatal("set default QRIS gagal")
	}
	expect(t, e.do("PUT", "/v1/me/payment-methods/default", token, map[string]any{"id": "PAYPAL"}), 404, "NOT_FOUND")
	expect(t, e.do("DELETE", "/v1/me/payment-methods/COD", token, nil), 422, "PAYMENT_METHOD_BUILTIN")

	// Hapus e-wallet utama → utama kembali ke COD.
	e.do("PUT", "/v1/me/payment-methods/default", token, map[string]any{"id": walletID})
	rm := e.do("DELETE", "/v1/me/payment-methods/"+walletID, token, nil)
	expect(t, rm, 200, "")
	if rm.data()["default_id"] != "COD" || len(rm.data()["methods"].([]any)) != 2 {
		t.Fatalf("setelah hapus: %v", rm.data())
	}
	expect(t, e.do("DELETE", "/v1/me/payment-methods/"+walletID, token, nil), 404, "NOT_FOUND")

	// User lain tidak bisa memakai e-wallet milik orang lain.
	other := e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "OVO", "account_number": "081111111111"})
	otherMethods := other.data()["methods"].([]any)
	otherID := otherMethods[len(otherMethods)-1].(map[string]any)["id"].(string)
	demo, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("PUT", "/v1/me/payment-methods/default", demo, map[string]any{"id": otherID}), 404, "NOT_FOUND")
	expect(t, e.do("DELETE", "/v1/me/payment-methods/"+otherID, demo, nil), 404, "NOT_FOUND")
	expect(t, e.do("GET", "/v1/me/payment-methods", "", nil), 401, "UNAUTHORIZED")

	// Checkout memakai e-wallet sekarang diizinkan.
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"store_id": "ST01", "payment_method": "EWALLET",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}}, "Idempotency-Key", "ewallet-1"), 201, "")

	// ===== Pusat bantuan (publik) =====
	help := e.do("GET", "/v1/help", "", nil)
	expect(t, help, 200, "")
	topics := help.data()["topics"].([]any)
	if len(topics) != 5 || len(help.data()["contacts"].([]any)) != 3 || help.data()["operating_hours"] == "" {
		t.Fatalf("help: %d topik, %v", len(topics), help.data()["contacts"])
	}
	found := e.do("GET", "/v1/help?q=e-wallet", "", nil).data()
	if len(found["topics"].([]any)) == 0 || found["query"] != "e-wallet" {
		t.Fatalf("cari e-wallet: %v", found)
	}
	if n := len(e.do("GET", "/v1/help?q=%25", "", nil).data()["topics"].([]any)); n != 0 {
		t.Fatalf("%% harus dianggap huruf biasa, dapat %d topik", n)
	}
	expect(t, e.do("GET", "/v1/help?q="+strings.Repeat("a", 101), "", nil), 400, "VALIDATION_ERROR")

	// ===== Dokumen legal (publik) =====
	privacy := e.do("GET", "/v1/legal/privacy-policy", "", nil)
	expect(t, privacy, 200, "")
	if privacy.data()["title"] != "Kebijakan Privasi" || len(privacy.data()["sections"].([]any)) != 6 || privacy.data()["effective_at"] != "2026-10-01" {
		t.Fatalf("privacy: %v", privacy.data())
	}
	expect(t, e.do("GET", "/v1/legal/terms", "", nil), 200, "")
	expect(t, e.do("GET", "/v1/legal/rahasia", "", nil), 404, "NOT_FOUND")

	// Menu Profil di app-config sudah menunjuk screen native.
	menu := e.do("GET", "/v1/app-config", "", nil).data()["profile_menu"].([]any)
	if menu[0].(map[string]any)["id"] != "edit_profile" {
		t.Fatalf("profile_menu: %v", menu[0])
	}
}

func TestCheckoutDefaults(t *testing.T) {
	e := setup(t)
	token, _ := e.login("kholis@gmail.com", "12345678")
	items := []map[string]any{{"product_id": "P01", "quantity": 1}}
	order := func(body map[string]any, key string) resp {
		body["items"] = items
		return e.do("POST", "/v1/orders", token, body, "Idempotency-Key", key)
	}

	// Tanpa payment & alamat → metode utama (COD) + alamat utama (seed: Rumah).
	r := order(map[string]any{}, "d-1")
	expect(t, r, 201, "")
	if r.data()["payment_method"] != "COD" || r.data()["payment_method_id"] != "COD" {
		t.Fatalf("default payment: %v / %v", r.data()["payment_method"], r.data()["payment_method_id"])
	}
	if addr, _ := r.data()["address"].(map[string]any); addr == nil || addr["label"] != "Rumah" {
		t.Fatalf("default address: %v", r.data()["address"])
	}

	// E-wallet jadi utama → pesanan tanpa payment memakai e-wallet + snapshot nomor tersamar.
	pm := e.do("POST", "/v1/me/payment-methods", token, map[string]any{"provider": "GOPAY", "account_number": "081234567890", "set_default": true})
	walletID := pm.data()["default_id"].(string)
	r = order(map[string]any{}, "d-2")
	expect(t, r, 201, "")
	if r.data()["payment_method"] != "EWALLET" || r.data()["payment_method_id"] != walletID ||
		r.data()["payment_provider"] != "GOPAY" || r.data()["payment_account_masked"] != "0812••••7890" {
		t.Fatalf("ewallet default: %v", r.data())
	}
	orderID := r.data()["order_id"].(string)

	// Pilih eksplisit: QRIS & alamat lain.
	a := e.do("POST", "/v1/me/addresses", token, map[string]any{"label": "Kantor", "recipient": "Kholis", "phone": "081234567890", "address": "Jl. Sudirman 1"})
	r = order(map[string]any{"payment_method_id": "QRIS", "address_id": a.data()["id"]}, "d-3")
	expect(t, r, 201, "")
	if r.data()["payment_method"] != "QRIS" || r.data()["address"].(map[string]any)["label"] != "Kantor" {
		t.Fatalf("explicit: %v", r.data())
	}

	// Error: id tidak dikenal / milik orang lain / tidak cocok dengan payment_method.
	expect(t, order(map[string]any{"payment_method_id": "PAYPAL"}, "d-4"), 404, "NOT_FOUND")
	demo, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("POST", "/v1/orders", demo, map[string]any{"payment_method_id": walletID, "items": items}), 404, "NOT_FOUND")
	expect(t, order(map[string]any{"payment_method_id": "QRIS", "payment_method": "COD"}, "d-5"), 400, "VALIDATION_ERROR")
	expect(t, order(map[string]any{"address_id": "00000000-0000-0000-0000-000000000000"}, "d-6"), 404, "NOT_FOUND")

	// E-wallet utama dihapus → default kembali COD; snapshot di pesanan lama tetap ada.
	e.do("DELETE", "/v1/me/payment-methods/"+walletID, token, nil)
	if r = order(map[string]any{}, "d-7"); r.data()["payment_method"] != "COD" {
		t.Fatalf("setelah hapus e-wallet: %v", r.data()["payment_method"])
	}
	old := e.do("GET", "/v1/orders/"+orderID, token, nil).data()
	if old["payment_account_masked"] != "0812••••7890" || old["payment_method_id"] != nil {
		t.Fatalf("snapshot pesanan lama: %v / id %v", old["payment_account_masked"], old["payment_method_id"])
	}

	// Demo belum punya alamat → pesanan tetap dibuat tanpa alamat.
	r = e.do("POST", "/v1/orders", demo, map[string]any{"items": items})
	expect(t, r, 201, "")
	if r.data()["address"] != nil {
		t.Fatalf("demo tanpa alamat: %v", r.data()["address"])
	}
}
