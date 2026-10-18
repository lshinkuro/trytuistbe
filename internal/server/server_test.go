package server_test

// Integration test: menjalankan router asli terhadap database Postgres sungguhan.
// Butuh TEST_DATABASE_URL (database KHUSUS test — isinya dihapus setiap run), mis.:
//
//	TEST_DATABASE_URL=postgres://trytuist:trytuist@localhost:5432/trytuist_test?sslmode=disable go test ./...

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
	app  *server.App
}

func setup(t *testing.T, opts ...server.Option) *env {
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
		GoogleClientIDs: []string{googleClientID}, GoogleJWKSURL: googleJWKS(t),
	}
	app := server.NewApp(cfg, pool, opts...)
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, pool: pool, app: app}
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
	// Filter waktu, urutan, can_rate
	canRate := func() map[string]bool {
		m := map[string]bool{}
		for _, o := range e.do("GET", "/v1/orders", token, nil).list() {
			m[o.(map[string]any)["order_id"].(string)] = o.(map[string]any)["can_rate"].(bool)
		}
		return m
	}
	if m := canRate(); !m[id2] || m[orderID] {
		t.Fatalf("can_rate salah sebelum dinilai: %v", m)
	}
	if asc := e.do("GET", "/v1/orders?sort=asc", token, nil).list(); len(asc) != 2 || asc[0].(map[string]any)["order_id"] != orderID {
		t.Fatalf("sort=asc harus pesanan terlama dulu: %v", asc)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if l := e.do("GET", "/v1/orders?from="+future, token, nil).list(); len(l) != 0 {
		t.Fatalf("from di masa depan harus kosong, dapat %d", len(l))
	}
	if l := e.do("GET", "/v1/orders?to="+future+"&status=COMPLETED", token, nil).list(); len(l) != 1 {
		t.Fatalf("to + status harus 1 pesanan, dapat %d", len(l))
	}
	expect(t, e.do("GET", "/v1/orders?from=kemarin", token, nil), 400, "VALIDATION_ERROR")
	expect(t, e.do("GET", "/v1/orders?sort=new", token, nil), 400, "VALIDATION_ERROR")

	expect(t, e.do("POST", "/v1/orders/"+id2+"/rating", token, map[string]any{"rating": 4, "comment": "Enak"}), 200, "")
	expect(t, e.do("POST", "/v1/orders/"+id2+"/rating", token, map[string]any{"rating": 4}), 409, "ALREADY_RATED")
	if canRate()[id2] {
		t.Fatal("can_rate harus false setelah dinilai")
	}
	for _, o := range e.do("GET", "/v1/orders?status=COMPLETED", token, nil).list() {
		if got := o.(map[string]any)["rating"]; got != float64(4) {
			t.Fatalf("rating di list = %v, harap 4", got)
		}
	}
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
		SET value = (SELECT jsonb_agg(CASE WHEN m->>'id' = 'EWALLET' THEN jsonb_set(m, '{is_enabled}', 'false') ELSE m END)
		             FROM jsonb_array_elements(value) m)
		WHERE key = 'payment_methods'`); err != nil {
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

// ===== Google Sign-In (kunci & JWKS palsu, tanpa internet) =====

const googleClientID = "test-ios.apps.googleusercontent.com"

var (
	googleKeyOnce sync.Once
	googleKey     *rsa.PrivateKey
)

func googleJWKS(t *testing.T) string {
	googleKeyOnce.Do(func() { googleKey, _ = rsa.GenerateKey(rand.Reader, 2048) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test-kid",
			"n": base64.RawURLEncoding.EncodeToString(googleKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(googleKey.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func googleIDToken(t *testing.T, sub, email, name string, mutate ...func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": "https://accounts.google.com", "aud": googleClientID, "sub": sub,
		"email": email, "email_verified": true, "name": name,
		"picture": "https://lh3.googleusercontent.com/a/" + sub,
		"iat":     time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for _, m := range mutate {
		m(claims)
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": "test-kid"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, googleKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestGoogleSignIn(t *testing.T) {
	e := setup(t)
	google := func(token string) resp {
		return e.do("POST", "/v1/auth/google", "", map[string]any{"id_token": token})
	}

	// 1. Daftar: akun baru → 201, is_new_user true, nama & foto dari Google.
	r := google(googleIDToken(t, "g-100", "Budi.Santoso@gmail.com", "Budi Santoso"))
	expect(t, r, 201, "")
	if r.data()["is_new_user"] != true || r.data()["user_name"] != "Budi Santoso" || r.data()["user_image"] != "https://lh3.googleusercontent.com/a/g-100" {
		t.Fatalf("daftar google: %v", r.data())
	}
	token := r.data()["access_token"].(string)
	me := e.do("GET", "/v1/me", token, nil).data()
	if me["email"] != "budi.santoso@gmail.com" {
		t.Fatalf("email harus lowercase: %v", me["email"])
	}

	// 2. Masuk lagi (sub sama, walau nama di Google berubah) → 200, akun yang sama.
	r = google(googleIDToken(t, "g-100", "budi.santoso@gmail.com", "Budi S"))
	expect(t, r, 200, "")
	if r.data()["is_new_user"] != false || e.do("GET", "/v1/me", r.data()["access_token"].(string), nil).data()["id"] != me["id"] {
		t.Fatal("login google kedua harus ke akun yang sama")
	}

	// 3. Akun Google tanpa password → login password ditolak dengan petunjuk.
	expect(t, e.do("POST", "/v1/auth/login", "", map[string]any{"email": "budi.santoso@gmail.com", "password": "apapun123"}),
		401, "USE_GOOGLE_SIGN_IN")
	// Register email+password dengan email yang sama → 409.
	expect(t, e.do("POST", "/v1/auth/register", "", map[string]any{"email": "budi.santoso@gmail.com", "password": "rahasia123"}),
		409, "EMAIL_ALREADY_REGISTERED")

	// 4. Email sudah terdaftar (akun password) → Google ditautkan, data lama tetap (nama Kholis, tier GOLD).
	r = google(googleIDToken(t, "g-200", "kholis@gmail.com", "Nama Google"))
	expect(t, r, 200, "")
	linked := e.do("GET", "/v1/me", r.data()["access_token"].(string), nil).data()
	if r.data()["is_new_user"] != false || linked["name"] != "Kholis" || linked["member_tier"] != "GOLD" {
		t.Fatalf("tautkan google ke akun lama: %v / %v", r.data(), linked)
	}
	// Password lama tetap bisa dipakai.
	e.login("kholis@gmail.com", "12345678")
	// Email sama dari akun Google lain → ditolak.
	expect(t, google(googleIDToken(t, "g-999", "kholis@gmail.com", "Penyusup")), 409, "EMAIL_ALREADY_REGISTERED")

	// 5. Token tidak valid.
	expect(t, google(googleIDToken(t, "g-300", "x@gmail.com", "X", func(c map[string]any) { c["aud"] = "app-lain" })), 401, "INVALID_GOOGLE_TOKEN")
	expect(t, google(googleIDToken(t, "g-300", "x@gmail.com", "X", func(c map[string]any) { c["email_verified"] = false })), 401, "INVALID_GOOGLE_TOKEN")
	expect(t, google(googleIDToken(t, "g-300", "x@gmail.com", "X", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() })), 401, "INVALID_GOOGLE_TOKEN")
	expect(t, google("bukan-token"), 401, "INVALID_GOOGLE_TOKEN")
	expect(t, e.do("POST", "/v1/auth/google", "", map[string]any{}), 400, "VALIDATION_ERROR")
}

func TestGoogleSignInDisabled(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL belum di-set; integration test dilewati")
	}
	pool, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	resetDB(t, pool)
	cfg := config.Config{Env: "dev", APIKeys: []string{apiKey}, JWTSecret: "test-secret-test-secret-test-secret",
		JWTAccessTTL: time.Minute, JWTRefreshTTL: time.Hour, Timezone: time.UTC, LoginRateLimit: 100}
	srv := httptest.NewServer(server.NewRouter(cfg, pool))
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, pool: pool}
	expect(t, e.do("POST", "/v1/auth/google", "", map[string]any{"id_token": "x.y.z"}), 503, "GOOGLE_SIGN_IN_DISABLED")
}

func TestProductDeals(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// Harga promo tampil di detail & list; harga normal di original_price.
	p := e.do("GET", "/v1/products/P07", "", nil).data()
	if p["price"].(float64) != 19900 || p["original_price"].(float64) != 32000 || p["discount_percent"].(float64) != 37 {
		t.Fatalf("harga promo P07 salah: %v", p)
	}
	if tags := fmt.Sprint(p["tags"]); !strings.Contains(tags, "promo") {
		t.Fatalf("tag promo tidak ada: %v", tags)
	}
	if p := e.do("GET", "/v1/products/P01", "", nil).data(); p["original_price"] != nil || p["discount_percent"].(float64) != 0 {
		t.Fatalf("P01 tidak sedang promo: %v", p)
	}
	cheapest := e.do("GET", "/v1/products?sort=price_asc&limit=1", "", nil).list()[0].(map[string]any)
	if cheapest["product_id"] != "P12" || cheapest["price"].(float64) != 12000 {
		t.Fatalf("sort harga harus pakai harga promo: %v", cheapest)
	}

	// Popup Beranda
	pop := e.do("GET", "/v1/promotions/popup", "", nil)
	expect(t, pop, 200, "")
	d := pop.data()
	prod, _ := d["product"].(map[string]any)
	if d["id"] != "D01" || d["badge"] != "HEMAT 37%" || prod["product_id"] != "P07" || d["image_url"] != prod["image_url"] || d["ends_at"] == "" {
		t.Fatalf("popup salah: %v", d)
	}

	// Quote & checkout memakai harga promo.
	token, _ := e.login("kholis@gmail.com", "12345678")
	q := e.do("POST", "/v1/orders/quote", token, map[string]any{"items": []map[string]any{{"product_id": "P07", "quantity": 2}}})
	expect(t, q, 200, "")
	if got := q.data()["subtotal"].(float64); got != 39800 {
		t.Fatalf("subtotal = %v, harap 39800 (2 × 19.900)", got)
	}

	// Produk habis → popup dilewati; deal lewat → harga normal & popup kosong.
	if _, err := e.pool.Exec(ctx, `UPDATE products SET is_available = FALSE WHERE id = 'P07'`); err != nil {
		t.Fatal(err)
	}
	if pop := e.do("GET", "/v1/promotions/popup", "", nil); pop.Body["data"] != nil {
		t.Fatalf("popup produk habis harus null: %v", pop.Body)
	}
	for _, sql := range []string{
		`UPDATE products SET is_available = TRUE WHERE id = 'P07'`,
		`UPDATE product_deals SET starts_at = now() - interval '2 days', ends_at = now() - interval '1 day' WHERE id = 'D01'`,
	} {
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if pop := e.do("GET", "/v1/promotions/popup", "", nil); pop.Body["data"] != nil {
		t.Fatalf("popup deal kedaluwarsa harus null: %v", pop.Body)
	}
	if p := e.do("GET", "/v1/products/P07", "", nil).data(); p["price"].(float64) != 32000 || p["original_price"] != nil {
		t.Fatalf("setelah promo berakhir harga harus normal: %v", p)
	}
}

func TestAdmin(t *testing.T) {
	e := setup(t)

	customer, _ := e.login("kholis@gmail.com", "12345678")
	expect(t, e.do("GET", "/v1/admin/me", "", nil), 401, "UNAUTHORIZED")
	expect(t, e.do("GET", "/v1/admin/me", customer, nil), 403, "FORBIDDEN")

	adminTok, _ := e.login("admin@trytuist.com", "admin12345")
	me := e.do("GET", "/v1/admin/me", adminTok, nil)
	expect(t, me, 200, "")
	if me.data()["role"] != "ADMIN" || me.data()["email"] != "admin@trytuist.com" {
		t.Fatalf("admin/me salah: %v", me.data())
	}

	// Dua pesanan dari pelanggan.
	o1 := e.do("POST", "/v1/orders", customer, map[string]any{"payment_method": "COD",
		"items": []map[string]any{{"product_id": "P01", "quantity": 2}}})
	expect(t, o1, 201, "")
	o2 := e.do("POST", "/v1/orders", customer, map[string]any{"payment_method": "QRIS",
		"items": []map[string]any{{"product_id": "P14", "quantity": 1}}})
	expect(t, o2, 201, "")
	id1, id2 := o1.data()["order_id"].(string), o2.data()["order_id"].(string)

	list := e.do("GET", "/v1/admin/orders?status=PENDING&q=kholis", adminTok, nil)
	expect(t, list, 200, "")
	if n := len(list.list()); n != 2 || list.Body["meta"].(map[string]any)["total"].(float64) != 2 {
		t.Fatalf("daftar pesanan admin = %d: %v", n, list.Body)
	}
	if c := list.list()[0].(map[string]any)["customer"].(map[string]any); c["email"] != "kholis@gmail.com" {
		t.Fatalf("customer salah: %v", c)
	}
	expect(t, e.do("GET", "/v1/admin/orders?status=DONE", adminTok, nil), 400, "VALIDATION_ERROR")
	if n := len(e.do("GET", "/v1/admin/orders?q=tidak-ada", adminTok, nil).list()); n != 0 {
		t.Fatalf("pencarian harus kosong, dapat %d", n)
	}

	detail := e.do("GET", "/v1/admin/orders/"+id1, adminTok, nil).data()
	if next := fmt.Sprint(detail["next_statuses"]); !strings.Contains(next, "COMPLETED") || !strings.Contains(next, "FAILED") {
		t.Fatalf("next_statuses = %v", next)
	}

	// Gagal: alasan wajib; status akhir terkunci; pelanggan melihat alasannya.
	expect(t, e.do("PATCH", "/v1/admin/orders/"+id1+"/status", adminTok, map[string]any{"status": "FAILED"}), 400, "VALIDATION_ERROR")
	failed := e.do("PATCH", "/v1/admin/orders/"+id1+"/status", adminTok, map[string]any{"status": "FAILED", "note": "Stok habis"})
	expect(t, failed, 200, "")
	hist := failed.data()["status_history"].([]any)
	last := hist[len(hist)-1].(map[string]any)
	if last["status"] != "FAILED" || last["note"] != "Stok habis" || last["changed_by"] != "Admin TryTuist" {
		t.Fatalf("riwayat status salah: %v", last)
	}
	expect(t, e.do("PATCH", "/v1/admin/orders/"+id1+"/status", adminTok, map[string]any{"status": "COMPLETED"}), 422, "INVALID_STATUS_TRANSITION")
	mine := e.do("GET", "/v1/orders/"+id1, customer, nil).data()
	if mine["status"] != "FAILED" {
		t.Fatalf("pelanggan harus melihat FAILED: %v", mine["status"])
	}

	// Sukses langsung dari PENDING.
	expect(t, e.do("PATCH", "/v1/admin/orders/"+id2+"/status", adminTok, map[string]any{"status": "COMPLETED"}), 200, "")
	expect(t, e.do("PATCH", "/v1/admin/orders/"+id2+"/status", customer, map[string]any{"status": "FAILED", "note": "x"}), 403, "FORBIDDEN")

	// Dashboard
	dash := e.do("GET", "/v1/admin/dashboard", adminTok, nil)
	expect(t, dash, 200, "")
	d := dash.data()
	sales, purchases := d["sales"].(map[string]any), d["purchases"].(map[string]any)
	if sales["revenue"].(float64) != o2.data()["total"].(float64) || sales["orders_completed"].(float64) != 1 || sales["items_sold"].(float64) != 1 {
		t.Fatalf("penjualan salah: %v", sales)
	}
	if purchases["orders"].(float64) != 2 || purchases["failed"].(float64) != 1 || purchases["buyers"].(float64) != 1 ||
		purchases["new_buyers"].(float64) != 1 || purchases["success_rate_pct"].(float64) != 50 {
		t.Fatalf("pembelian salah: %v", purchases)
	}
	if n := len(d["daily"].([]any)); n != 30 {
		t.Fatalf("daily = %d hari, harap 30", n)
	}
	if top := d["top_products"].([]any); len(top) != 1 || top[0].(map[string]any)["product_id"] != "P14" {
		t.Fatalf("top produk salah: %v", top)
	}
	expect(t, e.do("GET", "/v1/admin/dashboard?from=2026-05-01&to=2026-04-01", adminTok, nil), 400, "VALIDATION_ERROR")

	// Ubah harga & stok
	expect(t, e.do("PATCH", "/v1/admin/products/P01", adminTok, map[string]any{"price": -1}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/admin/products/P01", adminTok, map[string]any{}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/admin/products/P99", adminTok, map[string]any{"price": 1000}), 404, "NOT_FOUND")
	up := e.do("PATCH", "/v1/admin/products/P01", adminTok, map[string]any{"price": 23000})
	expect(t, up, 200, "")
	if p := e.do("GET", "/v1/products/P01", "", nil).data(); p["price"].(float64) != 23000 {
		t.Fatalf("harga publik belum berubah: %v", p["price"])
	}
	expect(t, e.do("PATCH", "/v1/admin/products/P01", adminTok, map[string]any{"is_available": false}), 200, "")
	if p := e.do("GET", "/v1/products/P01", "", nil).data(); p["is_available"] != false {
		t.Fatal("P01 harus habis")
	}
	if n := len(e.do("GET", "/v1/admin/products?available=false", adminTok, nil).list()); n != 2 { // P01 + P06
		t.Fatalf("produk habis = %d, harap 2", n)
	}

	// Tambah menu
	expect(t, e.do("POST", "/v1/admin/products", adminTok, map[string]any{"product_name": "Es Teh"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/admin/products", adminTok, map[string]any{"category_id": "nope", "product_name": "Es Teh", "price": 8000}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/admin/products", adminTok, map[string]any{"category_id": "non_coffee", "product_name": "Es Teh", "price": 8000,
		"options": []map[string]any{{"name": "Regular"}, {"name": "regular"}}}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/admin/products", adminTok, map[string]any{"category_id": "non_coffee", "product_name": "Es Teh", "price": 8000,
		"image_url": "http://x.com/a.jpg"}), 400, "VALIDATION_ERROR")
	menu := e.do("POST", "/v1/admin/products", adminTok, map[string]any{
		"category_id": "non_coffee", "product_name": "  Es Teh Manis ", "description": "Teh melati dingin.", "price": 8000,
		"image_url": "https://images.unsplash.com/photo-1556679343-c7306c1976bc?w=800",
		"options":   []map[string]any{{"name": "Regular"}, {"name": "Porsi Jumbo", "extra_price": 3000}},
	})
	expect(t, menu, 201, "")
	newID := menu.data()["product_id"].(string)
	if newID != "P15" || menu.data()["product_name"] != "Es Teh Manis" || len(menu.data()["options"].([]any)) != 2 {
		t.Fatalf("menu baru salah: %v", menu.data())
	}
	if opt := menu.data()["options"].([]any)[1].(map[string]any); opt["id"] != "porsi-jumbo" || opt["extra_price"].(float64) != 3000 {
		t.Fatalf("varian salah: %v", opt)
	}
	pub := e.do("GET", "/v1/products/"+newID, "", nil)
	expect(t, pub, 200, "")
	if pub.data()["price"].(float64) != 8000 || pub.data()["category_id"] != "non_coffee" {
		t.Fatalf("menu baru belum tampil di app: %v", pub.data())
	}
	if n := len(e.do("GET", "/v1/products?limit=100", "", nil).list()); n != 15 {
		t.Fatalf("produk = %d, harap 15", n)
	}
	// Bisa langsung dipesan pelanggan (varian baru ikut dihitung).
	q := e.do("POST", "/v1/orders/quote", customer, map[string]any{"items": []map[string]any{{"product_id": newID, "option_id": "porsi-jumbo", "quantity": 2}}})
	expect(t, q, 200, "")
	if q.data()["subtotal"].(float64) != 22000 {
		t.Fatalf("subtotal menu baru = %v, harap 22000", q.data()["subtotal"])
	}
	// Ubah varian & foto; kirim [] = hapus semua varian.
	edited := e.do("PATCH", "/v1/admin/products/"+newID, adminTok, map[string]any{"options": []map[string]any{}, "image_url": ""})
	expect(t, edited, 200, "")
	if len(edited.data()["options"].([]any)) != 0 || edited.data()["image_url"] != "" {
		t.Fatalf("edit varian/foto gagal: %v", edited.data())
	}
	if second := e.do("POST", "/v1/admin/products", adminTok, map[string]any{"category_id": "snack", "product_name": "Pisang Goreng", "price": 12000}); second.data()["product_id"] != "P16" {
		t.Fatalf("id menu kedua = %v", second.data()["product_id"])
	}

	// Promo
	expect(t, e.do("POST", "/v1/admin/deals", adminTok, map[string]any{"product_id": "P01", "title": "Kopi murah",
		"deal_price": 30000, "ends_at": time.Now().Add(48 * time.Hour)}), 422, "DEAL_PRICE_TOO_HIGH")
	expect(t, e.do("POST", "/v1/admin/deals", adminTok, map[string]any{"product_id": "P01"}), 400, "VALIDATION_ERROR")
	created := e.do("POST", "/v1/admin/deals", adminTok, map[string]any{"product_id": "P01", "title": "Kopi murah",
		"deal_price": 15000, "ends_at": time.Now().Add(48 * time.Hour)})
	expect(t, created, 201, "")
	dealID := created.data()["id"].(string)
	if created.data()["status"] != "ACTIVE" || created.data()["discount_percent"].(float64) != 34 {
		t.Fatalf("promo baru salah: %v", created.data())
	}
	if p := e.do("GET", "/v1/products/P01", "", nil).data(); p["price"].(float64) != 15000 || p["original_price"].(float64) != 23000 {
		t.Fatalf("harga promo belum berlaku: %v", p)
	}
	off := e.do("PATCH", "/v1/admin/deals/"+dealID, adminTok, map[string]any{"is_active": false})
	expect(t, off, 200, "")
	if off.data()["status"] != "INACTIVE" {
		t.Fatalf("status promo = %v", off.data()["status"])
	}
	if p := e.do("GET", "/v1/products/P01", "", nil).data(); p["price"].(float64) != 23000 {
		t.Fatalf("promo nonaktif harus kembali ke harga normal: %v", p["price"])
	}
	expect(t, e.do("DELETE", "/v1/admin/deals/"+dealID, adminTok, nil), 200, "")
	expect(t, e.do("DELETE", "/v1/admin/deals/"+dealID, adminTok, nil), 404, "NOT_FOUND")
	if n := len(e.do("GET", "/v1/admin/deals", adminTok, nil).list()); n != 3 { // seed D01–D03
		t.Fatalf("promo = %d, harap 3", n)
	}

	cust := e.do("GET", "/v1/admin/customers?q=kholis", adminTok, nil)
	expect(t, cust, 200, "")
	if c := cust.list(); len(c) != 1 || c[0].(map[string]any)["orders_count"].(float64) != 2 ||
		c[0].(map[string]any)["total_spent"].(float64) != o2.data()["total"].(float64) {
		t.Fatalf("pelanggan salah: %v", c)
	}

	logs := e.do("GET", "/v1/admin/audit-logs?limit=50", adminTok, nil)
	expect(t, logs, 200, "")
	actions := fmt.Sprint(logs.list())
	for _, a := range []string{"ORDER_STATUS", "PRODUCT_CREATE", "PRODUCT_UPDATE", "DEAL_CREATE", "DEAL_UPDATE", "DEAL_DELETE"} {
		if !strings.Contains(actions, a) {
			t.Fatalf("audit log tidak memuat %s", a)
		}
	}
}

func TestMultiOutlet(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	exec := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	find := func(list []any, id string) map[string]any {
		t.Helper()
		for _, x := range list {
			if m := x.(map[string]any); m["id"] == id || m["product_id"] == id || m["store_id"] == id {
				return m
			}
		}
		t.Fatalf("%s tidak ada di %v", id, list)
		return nil
	}

	// Daftar outlet dari lokasi Kebayoran Lama: Gandaria terdekat, Kelapa Gading di luar jangkauan.
	stores := e.do("GET", "/v1/stores?lat=-6.2420&lng=106.7830", "", nil).list()
	if len(stores) != 3 || stores[0].(map[string]any)["id"] != "ST01" {
		t.Fatalf("urutan outlet salah: %v", stores)
	}
	st01, st03 := find(stores, "ST01"), find(stores, "ST03")
	if st01["hours_label"] != "24 jam" || st01["is_deliverable"] != true || st03["is_deliverable"] != false {
		t.Fatalf("status outlet salah: %v | %v", st01, st03)
	}

	// Stok per outlet: Ramen habis di Kemang saja; harga sama.
	if p := e.do("GET", "/v1/products/P09?store_id=ST02", "", nil).data(); p["is_available"] != false {
		t.Fatalf("P09 harus habis di ST02: %v", p)
	}
	p01, p02 := e.do("GET", "/v1/products/P09", "", nil).data(), e.do("GET", "/v1/products/P09?store_id=ST01", "", nil).data()
	if p01["is_available"] != true || p02["is_available"] != true || p01["price"] != p02["price"] {
		t.Fatal("P09 tersedia di ST01 dengan harga yang sama")
	}
	if p := find(e.do("GET", "/v1/products?limit=100&store_id=ST02", "", nil).list(), "P09"); p["is_available"] != false {
		t.Fatal("list produk ST02 harus menandai P09 habis")
	}
	expect(t, e.do("GET", "/v1/products?store_id=NOPE", "", nil), 400, "VALIDATION_ERROR")
	home := e.do("GET", "/v1/home?store_id=ST02", "", nil).data()
	if home["store"].(map[string]any)["id"] != "ST02" || strings.Contains(fmt.Sprint(home["sections"]), "Ramen Pedas") {
		t.Fatalf("Beranda ST02 tidak boleh menampilkan menu yang habis: %v", home["store"])
	}

	// Checkout mengikuti outlet.
	token, _ := e.login("kholis@gmail.com", "12345678")
	addrs := e.do("GET", "/v1/me/addresses", token, nil).list()
	addrID := addrs[0].(map[string]any)["id"].(string)
	item := func(id string) []map[string]any { return []map[string]any{{"product_id": id, "quantity": 1}} }

	q := e.do("POST", "/v1/orders/quote", token, map[string]any{"address_id": addrID, "items": item("P09")})
	expect(t, q, 200, "")
	if q.data()["store_id"] != "ST01" || q.data()["store_name"] != "Gandaria City" {
		t.Fatalf("outlet otomatis harus Gandaria: %v", q.data())
	}
	bad := e.do("POST", "/v1/orders/quote", token, map[string]any{"store_id": "ST02", "address_id": addrID, "items": item("P09")})
	expect(t, bad, 422, "PRODUCT_UNAVAILABLE")
	if !strings.Contains(bad.Body["message"].(string), "Kemang Utara") {
		t.Fatalf("pesan harus menyebut outlet: %v", bad.Body["message"])
	}
	exec(`UPDATE stores SET opens_at = '00:00', closes_at = '00:00' WHERE id = 'ST03'`)
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"store_id": "ST03", "address_id": addrID, "items": item("P01")}),
		422, "OUT_OF_DELIVERY_AREA")
	// Tanpa alamat (ambil sendiri) jangkauan tidak dicek.
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"store_id": "ST03", "items": item("P01")}), 200, "")
	exec(`UPDATE stores SET opens_at = (now() AT TIME ZONE 'Asia/Jakarta' + interval '1 hour')::time,
		closes_at = (now() AT TIME ZONE 'Asia/Jakarta' + interval '2 hours')::time WHERE id = 'ST03'`)
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"store_id": "ST03", "items": item("P01")}), 422, "STORE_CLOSED")

	// Admin: tutup sementara → otomatis pindah ke outlet terdekat berikutnya.
	adminTok, _ := e.login("admin@trytuist.com", "admin12345")
	closed := e.do("PATCH", "/v1/admin/stores/ST01", adminTok, map[string]any{"is_temporarily_closed": true})
	expect(t, closed, 200, "")
	if closed.data()["is_open_now"] != false {
		t.Fatalf("ST01 harus tutup: %v", closed.data())
	}
	if s := find(e.do("GET", "/v1/stores?lat=-6.2420&lng=106.7830", "", nil).list(), "ST01"); s["is_open"] != false || s["closed_note"] == nil {
		t.Fatalf("ST01 harus terlihat tutup: %v", s)
	}
	expect(t, e.do("POST", "/v1/orders/quote", token, map[string]any{"store_id": "ST01", "items": item("P01")}), 422, "STORE_CLOSED")
	if q := e.do("POST", "/v1/orders/quote", token, map[string]any{"address_id": addrID, "items": item("P01")}); q.data()["store_id"] != "ST02" {
		t.Fatalf("saat Gandaria tutup harus pindah ke Kemang: %v", q.Body)
	}
	expect(t, e.do("PATCH", "/v1/admin/stores/ST01", adminTok, map[string]any{"is_temporarily_closed": false}), 200, "")
	expect(t, e.do("PATCH", "/v1/admin/stores/ST01", adminTok, map[string]any{"opens_at": "25:00"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/admin/stores/ST99", adminTok, map[string]any{"name": "X"}), 404, "NOT_FOUND")

	// Admin: tambah outlet & atur stoknya.
	expect(t, e.do("POST", "/v1/admin/stores", adminTok, map[string]any{"name": "Tebet"}), 400, "VALIDATION_ERROR")
	created := e.do("POST", "/v1/admin/stores", adminTok, map[string]any{
		"name": "Tebet", "address": "Jl. Tebet Raya No. 5", "lat": -6.2264, "lng": 106.8540,
		"opens_at": "09:00", "closes_at": "21:00", "delivery_radius_km": 4,
	})
	expect(t, created, 201, "")
	if created.data()["id"] != "ST04" || created.data()["hours_label"] != "09:00–21:00" {
		t.Fatalf("outlet baru salah: %v", created.data())
	}
	expect(t, e.do("PUT", "/v1/admin/stores/ST04/products/P01", adminTok, map[string]any{"is_available": false}), 200, "")
	if p := find(e.do("GET", "/v1/admin/products?store_id=ST04", adminTok, nil).list(), "P01"); p["store_available"] != false || p["store_sold_out"] != true {
		t.Fatalf("P01 harus habis di ST04: %v", p)
	}
	if p := e.do("GET", "/v1/products/P01?store_id=ST04", "", nil).data(); p["is_available"] != false {
		t.Fatal("app harus melihat P01 habis di ST04")
	}
	expect(t, e.do("PUT", "/v1/admin/stores/ST04/products/P01", adminTok, map[string]any{"is_available": true}), 200, "")
	if p := e.do("GET", "/v1/products/P01?store_id=ST04", "", nil).data(); p["is_available"] != true {
		t.Fatal("P01 harus tersedia lagi di ST04")
	}
	list := e.do("GET", "/v1/admin/stores", adminTok, nil).list()
	if len(list) != 4 || find(list, "ST02")["unavailable_products"].(float64) != 1 {
		t.Fatalf("daftar outlet admin salah: %v", list)
	}

	// Pesanan & dashboard per outlet.
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"store_id": "ST02", "payment_method": "COD", "items": item("P01")}), 201, "")
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"store_id": "ST01", "payment_method": "COD", "items": item("P02")}), 201, "")
	if o := e.do("GET", "/v1/admin/orders?store_id=ST02", adminTok, nil).list(); len(o) != 1 || o[0].(map[string]any)["store_id"] != "ST02" {
		t.Fatalf("filter pesanan per outlet salah: %v", o)
	}
	all := e.do("GET", "/v1/admin/dashboard", adminTok, nil).data()
	if all["store_id"] != nil || find(all["by_store"].([]any), "ST02")["orders"].(float64) != 1 {
		t.Fatalf("perbandingan outlet salah: %v", all["by_store"])
	}
	one := e.do("GET", "/v1/admin/dashboard?store_id=ST02", adminTok, nil).data()
	if one["purchases"].(map[string]any)["orders"].(float64) != 1 || len(one["by_store"].([]any)) != 0 {
		t.Fatalf("dashboard ST02 salah: %v", one["purchases"])
	}
	expect(t, e.do("GET", "/v1/admin/dashboard?store_id=NOPE", adminTok, nil), 400, "VALIDATION_ERROR")
}
