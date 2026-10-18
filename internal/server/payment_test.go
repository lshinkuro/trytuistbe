package server_test

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"trytuist-api/internal/payment"
	"trytuist-api/internal/server"
)

const fakeServerKey = "SB-Mid-server-test"

// fakeMidtrans: meniru Snap + Core API secukupnya. Status diatur tes lewat set().
type fakeMidtrans struct {
	mu       sync.Mutex
	snaps    map[string]payment.SnapRequest
	statuses map[string]payment.TransactionStatus
	expired  []string
	failSnap bool
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
