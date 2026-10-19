package server_test

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"trytuist-api/internal/config"
	"trytuist-api/internal/payment"
	"trytuist-api/internal/server"
)

const fakeServerKey = "SB-Mid-server-test"

// fakeMidtrans: meniru Snap + Core API secukupnya. Status diatur tes lewat set().
type fakeMidtrans struct {
	mu         sync.Mutex
	snaps      map[string]payment.SnapRequest
	statuses   map[string]payment.TransactionStatus
	expired    []string
	failSnap   bool
	refunds    []payment.RefundRequest
	failRefund string // isi → Refund ditolak dengan pesan ini
}

func newFakeMidtrans() *fakeMidtrans {
	return &fakeMidtrans{snaps: map[string]payment.SnapRequest{}, statuses: map[string]payment.TransactionStatus{}}
}

func (f *fakeMidtrans) CreateSnap(_ context.Context, req payment.SnapRequest) (payment.SnapResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSnap {
		return payment.SnapResponse{}, &payment.GatewayError{HTTPStatus: 500, Messages: "down"}
	}
	id := req.TransactionDetails.OrderID
	f.snaps[id] = req
	return payment.SnapResponse{Token: "tok-" + id, RedirectURL: "https://app.sandbox.midtrans.com/snap/v4/redirection/tok-" + id}, nil
}

func (f *fakeMidtrans) Status(_ context.Context, orderID string) (payment.TransactionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[orderID]
	if !ok {
		return st, payment.ErrTransactionNotFound
	}
	return st, nil
}

func (f *fakeMidtrans) Expire(_ context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, orderID)
	st, ok := f.statuses[orderID]
	if !ok {
		return payment.ErrTransactionNotFound
	}
	if st.TransactionStatus == "pending" {
		st.TransactionStatus = "expire"
		f.statuses[orderID] = st
	}
	return nil
}

func (f *fakeMidtrans) Refund(_ context.Context, orderID string, req payment.RefundRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRefund != "" {
		return &payment.GatewayError{HTTPStatus: 200, Messages: f.failRefund}
	}
	f.refunds = append(f.refunds, req)
	return nil
}

func (f *fakeMidtrans) VerifySignature(n payment.Notification) bool {
	return payment.VerifySignature(fakeServerKey, n)
}

func (f *fakeMidtrans) set(orderID, status, paymentType string, gross int64) payment.TransactionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := payment.TransactionStatus{
		OrderID: orderID, TransactionID: "txn-" + orderID, TransactionStatus: status, FraudStatus: "accept",
		PaymentType: paymentType, StatusCode: "200", GrossAmount: fmt.Sprintf("%d.00", gross),
	}
	f.statuses[orderID] = st
	return st
}

func signature(orderID, statusCode, gross string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + gross + fakeServerKey))
	return hex.EncodeToString(sum[:])
}

// notify: kirim webhook tanpa X-API-Key (seperti Midtrans).
func (e *env) notify(st payment.TransactionStatus, sig string) int {
	e.t.Helper()
	body := map[string]any{
		"order_id": st.OrderID, "transaction_id": st.TransactionID, "transaction_status": st.TransactionStatus,
		"fraud_status": st.FraudStatus, "payment_type": st.PaymentType, "status_code": st.StatusCode,
		"gross_amount": st.GrossAmount, "signature_key": sig,
	}
	b, _ := json.Marshal(body)
	res, err := http.Post(e.srv.URL+"/v1/payments/midtrans/notification", "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestPayments(t *testing.T) {
	gw := newFakeMidtrans()
	e := setup(t, server.WithPaymentGateway(gw))
	ctx := context.Background()
	token, _ := e.login("kholis@gmail.com", "12345678")
	cart := []map[string]any{{"product_id": "P01", "option_id": "regular", "quantity": 2}}
	online := map[string]any{"payment_method_id": "ONLINE", "items": cart}

	countOrders := func() int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	checkout := func(body map[string]any, key string) map[string]any {
		t.Helper()
		r := e.do("POST", "/v1/checkout", token, body, "Idempotency-Key", key)
		expect(t, r, 201, "")
		if r.data()["next"] != "PAYMENT" {
			t.Fatalf("harap next=PAYMENT: %v", r.Body)
		}
		return r.data()["payment"].(map[string]any)
	}
	getPayment := func(id string) map[string]any {
		t.Helper()
		r := e.do("GET", "/v1/payments/"+id, token, nil)
		expect(t, r, 200, "")
		return r.data()
	}

	// Metode bayar: Bayar Online muncul, QRIS disembunyikan.
	pm := e.do("GET", "/v1/me/payment-methods", token, nil).data()
	ids := []string{}
	for _, m := range pm["methods"].([]any) {
		mm := m.(map[string]any)
		if mm["is_enabled"] == true {
			ids = append(ids, mm["id"].(string))
		}
	}
	if fmt.Sprint(ids) != "[ONLINE COD]" {
		t.Fatalf("metode aktif harus [ONLINE COD], dapat %v", ids)
	}
	expect(t, e.do("PUT", "/v1/me/payment-methods/default", token, map[string]any{"id": "ONLINE"}), 200, "")

	// POST /v1/orders tidak bisa membuat pesanan online tanpa bayar; COD tetap langsung jadi.
	before := countOrders()
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"payment_method_id": "QRIS", "items": cart}), 422, "PAYMENT_REQUIRED")
	expect(t, e.do("POST", "/v1/orders", token, map[string]any{"items": cart}), 422, "PAYMENT_REQUIRED") // default = ONLINE
	cod := e.do("POST", "/v1/checkout", token, map[string]any{"payment_method_id": "COD", "items": cart})
	expect(t, cod, 201, "")
	if cod.data()["next"] != "ORDER" || cod.data()["order"].(map[string]any)["status"] != "PENDING" {
		t.Fatalf("COD lewat /v1/checkout harus langsung pesanan: %v", cod.Body)
	}
	if countOrders() != before+1 {
		t.Fatal("COD harus membuat 1 pesanan")
	}

	// ===== Sukses lewat webhook =====
	p := checkout(online, "key-1")
	pid, oid := p["id"].(string), p["provider_order_id"].(string)
	if p["status"] != "PENDING" || p["amount"] != float64(54000) || p["redirect_url"] == nil || p["order"] != nil {
		t.Fatalf("sesi baru salah: %v", p)
	}
	snap := gw.snaps[oid]
	var sum int64
	for _, it := range snap.ItemDetails {
		sum += it.Price * int64(it.Quantity)
	}
	if snap.TransactionDetails.GrossAmount != 54000 || sum != 54000 || snap.Expiry == nil || snap.Expiry.Duration != 15 {
		t.Fatalf("request Snap salah: %+v", snap)
	}
	if countOrders() != before+1 {
		t.Fatal("pesanan tidak boleh dibuat sebelum dibayar")
	}

	// Idempotency: key sama → sesi sama (200); isi beda → 409.
	again := e.do("POST", "/v1/checkout", token, online, "Idempotency-Key", "key-1")
	expect(t, again, 200, "")
	if again.data()["payment"].(map[string]any)["id"] != pid {
		t.Fatal("Idempotency-Key sama harus mengembalikan sesi yang sama")
	}
	other := map[string]any{"payment_method_id": "ONLINE", "items": []map[string]any{{"product_id": "P01", "quantity": 3}}}
	expect(t, e.do("POST", "/v1/checkout", token, other, "Idempotency-Key", "key-1"), 409, "IDEMPOTENCY_CONFLICT")

	// Belum memilih metode di Snap → tetap PENDING.
	if getPayment(pid)["status"] != "PENDING" {
		t.Fatal("harus masih PENDING")
	}

	// Signature salah → 403, tidak ada perubahan.
	st := gw.set(oid, "settlement", "gopay", 54000)
	if code := e.notify(st, "bukan-signature"); code != 403 {
		t.Fatalf("signature salah harus 403, dapat %d", code)
	}
	// Webhook settlement dikirim 3× → pesanan dibuat sekali.
	for i := 0; i < 3; i++ {
		if code := e.notify(st, signature(oid, "200", "54000.00")); code != 200 {
			t.Fatalf("webhook harus 200, dapat %d", code)
		}
	}
	paid := getPayment(pid)
	order, _ := paid["order"].(map[string]any)
	if paid["status"] != "PAID" || paid["payment_type"] != "gopay" || order == nil || order["status"] != "PAID" || paid["redirect_url"] != nil {
		t.Fatalf("harus PAID dengan pesanan: %v", paid)
	}
	if countOrders() != before+2 {
		t.Fatalf("pesanan harus dibuat tepat sekali, total %d", countOrders())
	}
	detail := e.do("GET", "/v1/orders/"+order["id"].(string), token, nil).data()
	hist := detail["status_history"].([]any)
	if detail["payment_method"] != "ONLINE" || detail["total"] != float64(54000) || detail["can_cancel"] != false ||
		hist[0].(map[string]any)["note"] != "Dibayar via GoPay" {
		t.Fatalf("detail pesanan online salah: %v", detail)
	}

	// Order_id tak dikenal (env lain) → 200 diabaikan.
	unknown := payment.TransactionStatus{OrderID: "TT-PROD-X", TransactionStatus: "settlement", StatusCode: "200", GrossAmount: "1000.00"}
	if code := e.notify(unknown, signature("TT-PROD-X", "200", "1000.00")); code != 200 {
		t.Fatalf("order_id tak dikenal harus 200, dapat %d", code)
	}

	// ===== Lunas lewat polling (tanpa webhook) =====
	p2 := checkout(online, "key-2")
	gw.set(p2["provider_order_id"].(string), "settlement", "qris", 54000)
	if got := getPayment(p2["id"].(string)); got["status"] != "PAID" || got["order"] == nil {
		t.Fatalf("polling harus menemukan pembayaran lunas: %v", got)
	}

	// ===== Jumlah tidak cocok → FAILED, tanpa pesanan =====
	n := countOrders()
	p3 := checkout(online, "key-3")
	oid3 := p3["provider_order_id"].(string)
	st3 := gw.set(oid3, "settlement", "bank_transfer", 1000)
	e.notify(st3, signature(oid3, "200", "1000.00"))
	if got := getPayment(p3["id"].(string)); got["status"] != "FAILED" || got["refund_status"] != "REQUIRED" || countOrders() != n {
		t.Fatalf("jumlah beda harus FAILED tanpa pesanan: %v", got)
	}

	// ===== Satu sesi aktif per user; batal; settlement terlambat =====
	a := checkout(online, "key-4")
	b := checkout(online, "key-5")
	if getPayment(a["id"].(string))["status"] != "CANCELLED" {
		t.Fatal("checkout baru harus membatalkan sesi PENDING lama")
	}
	pending := e.do("GET", "/v1/payments?status=PENDING", token, nil).list()
	if len(pending) != 1 || pending[0].(map[string]any)["id"] != b["id"] {
		t.Fatalf("hanya satu sesi PENDING: %v", pending)
	}
	cancelled := e.do("POST", "/v1/payments/"+b["id"].(string)+"/cancel", token, nil)
	expect(t, cancelled, 200, "")
	if cancelled.data()["status"] != "CANCELLED" {
		t.Fatalf("harus CANCELLED: %v", cancelled.Body)
	}
	expect(t, e.do("POST", "/v1/payments/"+b["id"].(string)+"/cancel", token, nil), 409, "PAYMENT_ALREADY_FINAL")
	// User ternyata tetap membayar halaman Snap lama → uang masuk → pesanan tetap dibuat.
	oidB := b["provider_order_id"].(string)
	stB := gw.set(oidB, "settlement", "shopeepay", 54000)
	e.notify(stB, signature(oidB, "200", "54000.00"))
	if got := getPayment(b["id"].(string)); got["status"] != "PAID" || got["order"] == nil {
		t.Fatalf("settlement terlambat harus tetap PAID: %v", got)
	}

	// ===== Kedaluwarsa oleh sweeper =====
	p6 := checkout(online, "key-6")
	if _, err := e.pool.Exec(ctx, `UPDATE payments SET expires_at = now() - interval '10 minutes' WHERE id = $1`, p6["id"]); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Payments.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getPayment(p6["id"].(string)); got["status"] != "EXPIRED" {
		t.Fatalf("sweeper harus EXPIRED: %v", got)
	}

	// Midtrans gangguan → 502, sesi FAILED.
	gw.failSnap = true
	expect(t, e.do("POST", "/v1/checkout", token, online, "Idempotency-Key", "key-7"), 502, "PAYMENT_GATEWAY_ERROR")
	gw.failSnap = false

	// Pesanan milik user lain tidak bisa dibaca.
	demo, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("GET", "/v1/payments/"+pid, demo, nil), 404, "NOT_FOUND")
}

func TestPaymentsDisabled(t *testing.T) {
	e := setup(t) // tanpa Midtrans
	token, _ := e.login("kholis@gmail.com", "12345678")
	r := e.do("POST", "/v1/checkout", token, map[string]any{"payment_method_id": "QRIS",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}})
	expect(t, r, 201, "")
	if r.data()["next"] != "ORDER" {
		t.Fatalf("tanpa Midtrans, checkout = pesanan langsung: %v", r.Body)
	}
	expect(t, e.do("POST", "/v1/checkout", token, map[string]any{"payment_method_id": "ONLINE",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}}), 422, "PAYMENT_METHOD_UNAVAILABLE")
	for _, m := range e.do("GET", "/v1/me/payment-methods", token, nil).data()["methods"].([]any) {
		if m.(map[string]any)["id"] == "ONLINE" {
			t.Fatal("ONLINE tidak boleh tampil bila Midtrans nonaktif")
		}
	}
}

// TestPaymentDevSimulate: debugger iOS bisa melunasi / menolak pembayaran tanpa simulator Midtrans.
func TestPaymentDevSimulate(t *testing.T) {
	gw := newFakeMidtrans()
	e := setup(t, server.WithPaymentGateway(gw))
	token, _ := e.login("kholis@gmail.com", "12345678")
	other, _ := e.login("demo@trytuist.com", "password123")
	online := map[string]any{"payment_method_id": "ONLINE",
		"items": []map[string]any{{"product_id": "P01", "option_id": "regular", "quantity": 1}}}
	newPayment := func(key string) string {
		t.Helper()
		r := e.do("POST", "/v1/checkout", token, online, "Idempotency-Key", key)
		expect(t, r, 201, "")
		return r.data()["payment"].(map[string]any)["id"].(string)
	}
	simulate := func(tok, id, status string) resp {
		return e.do("POST", "/v1/dev/payments/"+id+"/simulate", tok, map[string]any{"status": status, "payment_type": "gopay"})
	}

	id := newPayment("sim-1")
	expect(t, simulate(other, id, "settlement"), 404, "NOT_FOUND")
	expect(t, simulate(token, id, "lunas"), 400, "VALIDATION_ERROR")
	r := simulate(token, id, "settlement")
	expect(t, r, 200, "")
	if r.data()["status"] != "PAID" || r.data()["order"] == nil {
		t.Fatalf("simulasi lunas harus membuat pesanan: %v", r.data())
	}
	expect(t, simulate(token, id, "expire"), 422, "PAYMENT_NOT_PENDING")

	id = newPayment("sim-2")
	if s := simulate(token, id, "expire").data()["status"]; s != "EXPIRED" {
		t.Fatalf("simulasi kedaluwarsa = %v", s)
	}
}

// TestAdminPayments: web admin → Pembayaran (fase 5b): daftar, detail, refund penuh / sebagian / manual.
func TestAdminPayments(t *testing.T) {
	gw := newFakeMidtrans()
	e := setup(t, server.WithPaymentGateway(gw))
	token, _ := e.login("kholis@gmail.com", "12345678")
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	outlet, _ := e.login("kemang@trytuist.com", "outlet12345") // OUTLET_ADMIN Kemang Utara (ST02)
	online := map[string]any{"payment_method_id": "ONLINE", "store_id": "ST01",
		"items": []map[string]any{{"product_id": "P01", "option_id": "regular", "quantity": 2}}}
	pay := func(key, paymentType string) (string, map[string]any) {
		t.Helper()
		r := e.do("POST", "/v1/checkout", token, online, "Idempotency-Key", key)
		expect(t, r, 201, "")
		p := r.data()["payment"].(map[string]any)
		oid := p["provider_order_id"].(string)
		if paymentType != "" {
			st := gw.set(oid, "settlement", paymentType, 54000)
			if code := e.notify(st, signature(oid, "200", "54000.00")); code != 200 {
				t.Fatalf("webhook %d", code)
			}
		}
		return p["id"].(string), p
	}
	byID := func(list []any, id string) map[string]any {
		for _, x := range list {
			if m := x.(map[string]any); m["id"] == id {
				return m
			}
		}
		return nil
	}
	refund := func(tok, id string, body map[string]any) resp {
		return e.do("POST", "/v1/admin/payments/"+id+"/refund", tok, body)
	}

	pid, _ := pay("adm-1", "gopay")

	list := e.do("GET", "/v1/admin/payments?status=PAID", admin, nil)
	expect(t, list, 200, "")
	row := byID(list.list(), pid)
	if row == nil || row["order"] == nil || row["payment_type"] != "gopay" || row["customer"].(map[string]any)["email"] != "kholis@gmail.com" {
		t.Fatalf("daftar pembayaran salah: %v", row)
	}
	expect(t, e.do("GET", "/v1/admin/payments?status=LUNAS", admin, nil), 400, "VALIDATION_ERROR")
	if got := e.do("GET", "/v1/admin/payments", outlet, nil).list(); byID(got, pid) != nil {
		t.Fatal("admin outlet tidak boleh melihat pembayaran outlet lain")
	}
	expect(t, e.do("GET", "/v1/admin/payments/"+pid, outlet, nil), 404, "NOT_FOUND")
	expect(t, refund(outlet, pid, map[string]any{"reason": "x"}), 403, "FORBIDDEN")

	d := e.do("GET", "/v1/admin/payments/"+pid, admin, nil).data()
	if d["refundable_amount"] != float64(54000) || d["can_refund_online"] != true || len(d["events"].([]any)) == 0 {
		t.Fatalf("detail pembayaran salah: %v", d)
	}
	orderID := d["order"].(map[string]any)["id"].(string)
	if op := e.do("GET", "/v1/admin/orders/"+orderID, admin, nil).data()["payment"]; op == nil || op.(map[string]any)["id"] != pid {
		t.Fatalf("detail pesanan admin harus berisi info pembayaran: %v", op)
	}

	// Validasi jumlah & alasan.
	expect(t, refund(admin, pid, map[string]any{"amount": 60000, "reason": "kelebihan"}), 422, "REFUND_AMOUNT_INVALID")
	expect(t, refund(admin, pid, map[string]any{"amount": 1000}), 400, "VALIDATION_ERROR")

	// Sebagian lewat Midtrans.
	r := refund(admin, pid, map[string]any{"amount": 20000, "reason": "Menu habis"})
	expect(t, r, 200, "")
	if r.data()["refund_status"] != "PARTIAL_REFUND" || r.data()["refunded_amount"] != float64(20000) || r.data()["refundable_amount"] != float64(34000) {
		t.Fatalf("refund sebagian salah: %v", r.data())
	}
	customerRefund := func() map[string]any {
		r, _ := e.do("GET", "/v1/orders/"+orderID, token, nil).data()["refund"].(map[string]any)
		return r
	}
	if r := customerRefund(); r == nil || r["status"] != "PARTIAL" || r["amount"] != float64(20000) {
		t.Fatalf("pelanggan harus melihat refund sebagian: %v", r)
	}
	if len(gw.refunds) != 1 || gw.refunds[0].Amount != 20000 || gw.refunds[0].RefundKey != r.data()["refunds"].([]any)[0].(map[string]any)["id"] {
		t.Fatalf("request ke Midtrans salah: %+v", gw.refunds)
	}

	// Ditolak Midtrans → 422, tercatat FAILED, sisa tidak berkurang.
	gw.failRefund = "412 Merchant cannot modify the status of the transaction"
	expect(t, refund(admin, pid, map[string]any{"reason": "coba"}), 422, "REFUND_REJECTED")
	gw.failRefund = ""
	d = e.do("GET", "/v1/admin/payments/"+pid, admin, nil).data()
	if d["refundable_amount"] != float64(34000) || d["refunds"].([]any)[1].(map[string]any)["status"] != "FAILED" {
		t.Fatalf("refund gagal tidak boleh mengurangi sisa: %v", d)
	}

	// Sisa penuh (amount kosong) → REFUND; setelah itu tidak ada sisa.
	r = refund(admin, pid, map[string]any{"reason": "Pesanan dibatalkan outlet"})
	expect(t, r, 200, "")
	if r.data()["refund_status"] != "REFUND" || r.data()["refundable_amount"] != float64(0) || gw.refunds[1].Amount != 34000 {
		t.Fatalf("refund penuh salah: %v", r.data())
	}
	expect(t, refund(admin, pid, map[string]any{"reason": "lagi"}), 422, "REFUND_AMOUNT_INVALID")
	if r := customerRefund(); r["status"] != "COMPLETED" || r["amount"] != float64(54000) {
		t.Fatalf("pelanggan harus melihat refund penuh: %v", r)
	}
	if got := e.do("GET", "/v1/admin/payments?refund=1", admin, nil).list(); byID(got, pid) == nil {
		t.Fatal("filter refund=1 harus memuat pembayaran yang direfund")
	}

	// VA tidak bisa direfund lewat API → manual.
	va, _ := pay("adm-2", "bank_transfer")
	expect(t, refund(admin, va, map[string]any{"reason": "x"}), 422, "REFUND_NOT_SUPPORTED")
	r = refund(admin, va, map[string]any{"reason": "Sudah ditransfer balik", "manual": true})
	expect(t, r, 200, "")
	if r.data()["refund_status"] != "REFUND" || len(gw.refunds) != 2 {
		t.Fatalf("refund manual tidak boleh memanggil Midtrans: %v", r.data())
	}

	// Belum lunas → 409.
	pending, _ := pay("adm-3", "")
	expect(t, refund(admin, pending, map[string]any{"reason": "x"}), 409, "PAYMENT_NOT_PAID")

	if logs := e.do("GET", "/v1/admin/audit-logs", admin, nil).list(); !strings.Contains(fmt.Sprint(logs), "PAYMENT_REFUND") {
		t.Fatal("refund harus tercatat di audit log")
	}
}

// TestAdminDeliverySettings: super admin melihat & mengubah aturan ongkir; quote langsung mengikuti.
func TestAdminDeliverySettings(t *testing.T) {
	e := setup(t)
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	outlet, _ := e.login("kemang@trytuist.com", "outlet12345")
	customer, _ := e.login("kholis@gmail.com", "12345678")

	expect(t, e.do("GET", "/v1/admin/delivery-settings", outlet, nil), 403, "FORBIDDEN")
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", outlet, map[string]any{"delivery_fee": 1}), 403, "FORBIDDEN")

	got := e.do("GET", "/v1/admin/delivery-settings", admin, nil)
	expect(t, got, 200, "")
	s := got.data()["settings"].(map[string]any)
	if s["delivery_fee"] != float64(10000) || s["delivery_fee_per_km"] != float64(2000) || len(got.data()["examples"].([]any)) == 0 {
		t.Fatalf("aturan awal (migrasi 0012) salah: %v", got.data())
	}

	// Pratinjau tidak menyimpan: 5 km dengan Rp 8.000 s.d. 2 km + Rp 3.000/km = 8.000 + 3×3.000 = 17.000.
	pv := e.do("POST", "/v1/admin/delivery-settings/preview", admin, map[string]any{
		"delivery_fee": 8000, "delivery_base_km": 2, "delivery_fee_per_km": 3000, "preview_distances": []float64{5}})
	expect(t, pv, 200, "")
	if ex := pv.data()["examples"].([]any)[0].(map[string]any); ex["delivery_fee"] != float64(17000) {
		t.Fatalf("pratinjau 5 km = %v, harap 17000", ex["delivery_fee"])
	}
	if e.do("GET", "/v1/admin/delivery-settings", admin, nil).data()["settings"].(map[string]any)["delivery_fee"] != float64(10000) {
		t.Fatal("pratinjau tidak boleh menyimpan")
	}

	// Validasi.
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"courier_speed_kmh": 0}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"delivery_fee_per_km": -1}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"delivery_fee_max": 5000}), 400, "VALIDATION_ERROR")

	// Simpan flat Rp 7.000 → quote pelanggan & /v1/app-config ikut berubah; key order_rules lain tetap.
	up := e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"delivery_fee": 7000, "delivery_fee_per_km": 0})
	expect(t, up, 200, "")
	if up.data()["updated_at"] == nil {
		t.Fatal("updated_at harus terisi")
	}
	addrID := e.do("GET", "/v1/me/addresses", customer, nil).list()[0].(map[string]any)["id"].(string)
	q := e.do("POST", "/v1/orders/quote", customer, map[string]any{"address_id": addrID,
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}})
	expect(t, q, 200, "")
	if q.data()["delivery_fee"] != float64(7000) {
		t.Fatalf("quote harus memakai ongkir baru: %v", q.data()["delivery_fee"])
	}
	rules := e.do("GET", "/v1/app-config", "", nil).data()["order_rules"].(map[string]any)
	if rules["delivery_fee"] != float64(7000) || rules["max_quantity"] != float64(20) {
		t.Fatalf("app-config salah: %v", rules)
	}
	if logs := e.do("GET", "/v1/admin/audit-logs", admin, nil).list(); !strings.Contains(fmt.Sprint(logs), "DELIVERY_SETTINGS") {
		t.Fatal("perubahan ongkir harus tercatat di audit log")
	}
}

// TestFreeDeliveryCoupon: 2 kupon gratis ongkir per minggu, dipakai hanya bila diaktifkan saat checkout.
func TestFreeDeliveryCoupon(t *testing.T) {
	gw := newFakeMidtrans()
	e := setup(t, server.WithPaymentGateway(gw))
	token, _ := e.login("kholis@gmail.com", "12345678")
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	addrID := e.do("GET", "/v1/me/addresses", token, nil).list()[0].(map[string]any)["id"].(string)
	body := func(use bool, method string) map[string]any {
		return map[string]any{"address_id": addrID, "use_free_delivery": use, "payment_method_id": method,
			"items": []map[string]any{{"product_id": "P01", "option_id": "regular", "quantity": 1}}}
	}
	fd := func(r resp) map[string]any { return r.data()["free_delivery"].(map[string]any) }

	// Tanpa diaktifkan: ongkir normal, kupon tidak terpakai.
	plain := e.do("POST", "/v1/orders/quote", token, body(false, ""))
	expect(t, plain, 200, "")
	fee := plain.data()["delivery_fee"].(float64)
	if fee <= 0 || fd(plain)["enabled"] != true || fd(plain)["remaining"] != float64(2) || fd(plain)["applied"] != false {
		t.Fatalf("quote awal salah: fee=%v %v", fee, fd(plain))
	}

	// Diaktifkan: ongkir 0, sisa setelah pesanan ini = 1.
	q := e.do("POST", "/v1/orders/quote", token, body(true, ""))
	expect(t, q, 200, "")
	if q.data()["delivery_fee"] != float64(0) || fd(q)["applied"] != true || fd(q)["fee_waived"] != fee || fd(q)["remaining"] != float64(1) ||
		q.data()["total"] != plain.data()["total"].(float64)-fee {
		t.Fatalf("kupon tidak diterapkan: %v", q.data())
	}

	// Dua pesanan COD memakai kupon → kuota habis.
	o1 := e.do("POST", "/v1/orders", token, body(true, "COD"))
	expect(t, o1, 201, "")
	if o1.data()["free_delivery_coupon"] != true || o1.data()["delivery_fee"] != float64(0) || o1.data()["delivery_fee_waived"] != fee {
		t.Fatalf("pesanan harus mencatat kupon: %v", o1.data())
	}
	o2 := e.do("POST", "/v1/orders", token, body(true, "COD"))
	expect(t, o2, 201, "")
	expect(t, e.do("POST", "/v1/orders/quote", token, body(true, "")), 422, "FREE_DELIVERY_QUOTA_EXCEEDED")
	expect(t, e.do("POST", "/v1/orders", token, body(true, "COD")), 422, "FREE_DELIVERY_QUOTA_EXCEEDED")
	if r := e.do("POST", "/v1/orders/quote", token, body(false, "")); fd(r)["remaining"] != float64(0) || r.data()["delivery_fee"] != fee {
		t.Fatalf("tanpa kupon tetap bisa pesan dengan ongkir normal: %v", r.data())
	}

	// Pesanan dibatalkan → kupon kembali.
	expect(t, e.do("POST", "/v1/orders/"+o2.data()["order_id"].(string)+"/cancel", token, nil), 200, "")
	if r := e.do("POST", "/v1/orders/quote", token, body(false, "")); fd(r)["remaining"] != float64(1) {
		t.Fatalf("kupon pesanan batal harus kembali: %v", fd(r))
	}

	// Sesi bayar online PENDING ikut dihitung (bisa terbayar belakangan)…
	p := e.do("POST", "/v1/checkout", token, body(true, "ONLINE"), "Idempotency-Key", "fd-1")
	expect(t, p, 201, "")
	expect(t, e.do("POST", "/v1/orders", token, body(true, "COD")), 422, "FREE_DELIVERY_QUOTA_EXCEEDED")
	// …kecuali saat checkout online baru, yang memang membatalkan sesi lama.
	expect(t, e.do("POST", "/v1/checkout", token, body(true, "ONLINE"), "Idempotency-Key", "fd-2"), 201, "")

	// Minggu lalu tidak dihitung.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE orders SET created_at = now() - interval '8 days'; UPDATE payments SET created_at = now() - interval '8 days'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/orders/quote", token, body(false, "")); fd(r)["remaining"] != float64(2) {
		t.Fatalf("kuota minggu baru harus penuh: %v", fd(r))
	}

	// Ongkir sudah 0 (gratis karena subtotal) → kupon tidak terpakai.
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"free_delivery_min_subtotal": 1}), 200, "")
	if r := e.do("POST", "/v1/orders/quote", token, body(true, "")); fd(r)["applied"] != false || fd(r)["remaining"] != float64(2) {
		t.Fatalf("kupon tidak boleh terpakai bila ongkir sudah 0: %v", fd(r))
	}

	// Fitur dimatikan super admin.
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"free_delivery_coupon_weekly": 0}), 400, "VALIDATION_ERROR")
	off := e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"free_delivery_coupon_enabled": false, "free_delivery_min_subtotal": 0})
	expect(t, off, 200, "")
	if off.data()["settings"].(map[string]any)["free_delivery_coupon_enabled"] != false {
		t.Fatalf("pengaturan kupon harus tersimpan: %v", off.data())
	}
	expect(t, e.do("POST", "/v1/orders/quote", token, body(true, "")), 422, "FREE_DELIVERY_UNAVAILABLE")
	if r := e.do("POST", "/v1/orders/quote", token, body(false, "")); fd(r)["enabled"] != false || fd(r)["remaining"] != float64(0) {
		t.Fatalf("fitur nonaktif: %v", fd(r))
	}
}

// TestCombosAndBanners: combo (paket isi tetap) & banner yang di-tap mengarah ke sesuatu yang nyata.
func TestCombosAndBanners(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	token, _ := e.login("kholis@gmail.com", "12345678")
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	stock := func(store, product string) *int {
		var n *int
		if err := e.pool.QueryRow(ctx, `SELECT stock FROM store_products WHERE store_id = $1 AND product_id = $2`, store, product).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cart := func(store string, items ...map[string]any) map[string]any {
		return map[string]any{"store_id": store, "payment_method_id": "COD", "items": items}
	}
	item := func(id string, qty int) map[string]any { return map[string]any{"product_id": id, "quantity": qty} }

	// Combo di katalog: isi, total satuan, hemat.
	c := e.do("GET", "/v1/products/C01?store_id=ST01", "", nil).data()
	combo, _ := c["combo"].(map[string]any)
	if combo == nil || len(combo["items"].([]any)) != 2 || combo["separate_price"] != float64(48000) || combo["savings"] != float64(9000) ||
		!strings.Contains(fmt.Sprint(c["tags"]), "combo") {
		t.Fatalf("combo salah: %v", c)
	}
	if e.do("GET", "/v1/products/P01", "", nil).data()["combo"] != nil {
		t.Fatal("produk biasa tidak punya combo")
	}

	// Isi habis di outlet → combo ikut habis & tidak bisa dipesan.
	exec(`UPDATE store_products SET is_available = FALSE WHERE store_id = 'ST01' AND product_id = 'P13'`)
	if e.do("GET", "/v1/products/C01?store_id=ST01", "", nil).data()["is_available"] != false {
		t.Fatal("combo harus habis bila isinya habis")
	}
	expect(t, e.do("POST", "/v1/orders/quote", token, cart("ST01", item("C01", 1))), 422, "PRODUCT_UNAVAILABLE")

	// Stok isi dihitung bersama pembelian satuan.
	exec(`UPDATE store_products SET stock = 1 WHERE store_id = 'ST02' AND product_id = 'P13'`)
	expect(t, e.do("POST", "/v1/orders/quote", token, cart("ST02", item("C01", 2))), 422, "OUT_OF_STOCK")
	expect(t, e.do("POST", "/v1/orders/quote", token, cart("ST02", item("C01", 1), item("P13", 1))), 422, "OUT_OF_STOCK")

	// Pesan combo → stok isi berkurang, isi paket tersimpan di pesanan; batal → stok kembali.
	o := e.do("POST", "/v1/orders", token, cart("ST02", item("C01", 1)))
	expect(t, o, 201, "")
	line := o.data()["items"].([]any)[0].(map[string]any)
	if line["unit_price"] != float64(39000) || len(line["bundle_items"].([]any)) != 2 {
		t.Fatalf("baris combo harus berisi isi paket: %v", line)
	}
	if n := stock("ST02", "P13"); n == nil || *n != 0 {
		t.Fatalf("stok croissant harus 0 setelah combo dipesan, dapat %v", n)
	}
	if e.do("GET", "/v1/products/C01?store_id=ST02", "", nil).data()["is_available"] != false {
		t.Fatal("combo habis saat stok isinya habis")
	}
	expect(t, e.do("POST", "/v1/orders/"+o.data()["order_id"].(string)+"/cancel", token, nil), 200, "")
	if n := stock("ST02", "P13"); n == nil || *n != 1 {
		t.Fatalf("stok isi harus kembali setelah batal, dapat %v", n)
	}

	// Banner: semuanya punya aksi; banner kupon hilang bila fitur kupon dimatikan.
	banners := e.do("GET", "/v1/banners", "", nil).list()
	actions := map[string]any{}
	for _, b := range banners {
		bm := b.(map[string]any)
		a := bm["action"].(map[string]any)
		actions[bm["id"].(string)] = fmt.Sprint(a["type"], ":", a["value"])
	}
	if actions["B1"] != "PRODUCT:P07" || actions["B2"] != "FREE_DELIVERY:<nil>" || actions["B3"] != "CATEGORY:combo" {
		t.Fatalf("aksi banner salah: %v", actions)
	}
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"free_delivery_coupon_enabled": false}), 200, "")
	for _, b := range e.do("GET", "/v1/banners", "", nil).list() {
		if b.(map[string]any)["id"] == "B2" {
			t.Fatal("banner kupon tidak boleh tampil saat kupon dimatikan")
		}
	}
}

// TestAdminBannersAndCombos: super admin mengelola banner & isi paket dari web admin.
func TestAdminBannersAndCombos(t *testing.T) {
	e := setup(t)
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	outlet, _ := e.login("kemang@trytuist.com", "outlet12345")
	img := "https://images.unsplash.com/photo-1?w=1200"
	publicIDs := func() string {
		ids := []string{}
		for _, b := range e.do("GET", "/v1/banners", "", nil).list() {
			ids = append(ids, b.(map[string]any)["id"].(string))
		}
		return strings.Join(ids, ",")
	}

	expect(t, e.do("GET", "/v1/admin/banners", outlet, nil), 403, "FORBIDDEN")
	list := e.do("GET", "/v1/admin/banners", admin, nil)
	expect(t, list, 200, "")
	b1 := byIDList(list.list(), "B1")
	if b1 == nil || b1["action_label"] != "Nasi Goreng Spesial" || b1["status"] != "LIVE" {
		t.Fatalf("banner B1 salah: %v", b1)
	}

	expect(t, e.do("POST", "/v1/admin/banners", admin, map[string]any{"title": "Tanpa gambar"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/admin/banners", admin, map[string]any{"title": "X", "image_url": img, "action_type": "PRODUCT"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/admin/banners", admin, map[string]any{"title": "X", "image_url": img, "action_type": "CATEGORY", "action_value": "nope"}), 400, "VALIDATION_ERROR")
	c := e.do("POST", "/v1/admin/banners", admin, map[string]any{"title": "Paket baru", "image_url": img,
		"action_type": "CATEGORY", "action_value": "combo", "sort_order": 0})
	expect(t, c, 201, "")
	id := c.data()["id"].(string)
	if !strings.HasPrefix(publicIDs(), id) {
		t.Fatalf("banner baru (urutan 0) harus tampil paling depan: %s", publicIDs())
	}
	if u := e.do("PATCH", "/v1/admin/banners/"+id, admin, map[string]any{"is_active": false}); u.data()["status"] != "INACTIVE" ||
		strings.Contains(publicIDs(), id) {
		t.Fatalf("banner nonaktif tidak boleh tampil: %v", u.data())
	}
	expect(t, e.do("PATCH", "/v1/admin/delivery-settings", admin, map[string]any{"free_delivery_coupon_enabled": false}), 200, "")
	if b2 := byIDList(e.do("GET", "/v1/admin/banners", admin, nil).list(), "B2"); b2["status"] != "HIDDEN" {
		t.Fatalf("banner kupon saat kupon mati = %v, harap HIDDEN", b2["status"])
	}
	expect(t, e.do("DELETE", "/v1/admin/banners/"+id, admin, nil), 200, "")
	expect(t, e.do("PATCH", "/v1/admin/banners/"+id, admin, map[string]any{"title": "x"}), 404, "NOT_FOUND")

	// Combo dari web admin.
	bundle := []map[string]any{{"product_id": "P01", "option_id": "large", "quantity": 1}, {"product_id": "P12", "quantity": 2}}
	p := e.do("POST", "/v1/admin/products", admin, map[string]any{"category_id": "combo", "product_name": "Paket Ngemil",
		"price": 50000, "bundle_items": bundle})
	expect(t, p, 201, "")
	pid := p.data()["product_id"].(string)
	if len(p.data()["bundle_items"].([]any)) != 2 || p.data()["separate_price"] != float64(27000+2*18000) {
		t.Fatalf("isi paket salah: %v", p.data())
	}
	if e.do("GET", "/v1/products/"+pid, "", nil).data()["combo"] == nil {
		t.Fatal("paket buatan admin harus tampil sebagai combo di app")
	}
	bad := func(items []map[string]any) resp {
		return e.do("PATCH", "/v1/admin/products/"+pid, admin, map[string]any{"bundle_items": items})
	}
	expect(t, bad([]map[string]any{{"product_id": "C01", "quantity": 1}}), 400, "VALIDATION_ERROR")                                // paket di dalam paket
	expect(t, bad([]map[string]any{{"product_id": "P12", "option_id": "large", "quantity": 1}}), 400, "VALIDATION_ERROR")          // varian salah
	expect(t, bad([]map[string]any{{"product_id": pid, "quantity": 1}}), 400, "VALIDATION_ERROR")                                  // dirinya sendiri
	expect(t, e.do("PATCH", "/v1/admin/products/P01", admin, map[string]any{"bundle_items": bundle[1:]}), 400, "VALIDATION_ERROR") // P01 isi C01
	expect(t, bad([]map[string]any{}), 200, "")
	if e.do("GET", "/v1/products/"+pid, "", nil).data()["combo"] != nil {
		t.Fatal("bundle_items kosong = kembali jadi produk biasa")
	}
}

func byIDList(list []any, id string) map[string]any {
	for _, x := range list {
		if m := x.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

// TestAbuseProtection: Idempotency-Key wajib & rate limit request yang mengubah data, checkout, dan daftar.
func TestAbuseProtection(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.CheckoutRateLimit, c.AuthRateLimit, c.WriteRateLimit = 2, 2, 6 })
	token, _ := e.login("kholis@gmail.com", "12345678")
	cart := map[string]any{"store_id": "ST01", "payment_method_id": "COD", "items": []map[string]any{{"product_id": "P01", "quantity": 1}}}

	// Tanpa Idempotency-Key → 400, tidak ada pesanan dibuat.
	expect(t, e.do("POST", "/v1/orders", token, cart, "Idempotency-Key", ""), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/checkout", token, cart, "Idempotency-Key", ""), 400, "VALIDATION_ERROR")

	// Checkout: 2/menit per user (dua 400 di atas sudah dihitung) → berikutnya 429.
	expect(t, e.do("POST", "/v1/checkout", token, cart), 429, "RATE_LIMITED")
	other, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("POST", "/v1/checkout", other, cart), 201, "")

	// Request yang mengubah data: 6/menit per user (3 POST di atas, termasuk yang ditolak); GET tidak dihitung.
	for _, name := range []string{"Kholis A", "Kholis B", "Kholis C"} {
		expect(t, e.do("PATCH", "/v1/me", token, map[string]any{"name": name}), 200, "")
	}
	expect(t, e.do("PATCH", "/v1/me", token, map[string]any{"name": "Kholis D"}), 429, "RATE_LIMITED")
	expect(t, e.do("GET", "/v1/me", token, nil), 200, "")

	// Daftar akun: 2/menit per IP.
	reg := func(email string) resp {
		return e.do("POST", "/v1/auth/register", "", map[string]any{"email": email, "password": "rahasia123", "name": "Spam"})
	}
	expect(t, reg("a1@example.com"), 201, "")
	expect(t, reg("a2@example.com"), 201, "")
	expect(t, reg("a3@example.com"), 429, "RATE_LIMITED")
}
