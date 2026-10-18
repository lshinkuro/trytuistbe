package server_test

import (
	"context"
	"sync"
	"testing"

	"trytuist-api/internal/push"
	"trytuist-api/internal/server"
)

type fakeAPNs struct {
	mu   sync.Mutex
	sent []struct {
		Device  push.Device
		Message push.Message
	}
	dead map[string]bool
}

func (f *fakeAPNs) Send(_ context.Context, d push.Device, m push.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead[d.Token] {
		return push.ErrUnregistered
	}
	f.sent = append(f.sent, struct {
		Device  push.Device
		Message push.Message
	}{d, m})
	return nil
}

func (f *fakeAPNs) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, s := range f.sent {
		out = append(out, s.Message.Title)
	}
	return out
}

func TestPush(t *testing.T) {
	apns := &fakeAPNs{dead: map[string]bool{}}
	gw := newFakeMidtrans()
	e := setup(t, server.WithPaymentGateway(gw), server.WithPushSender(apns))
	ctx := context.Background()
	token, _ := e.login("kholis@gmail.com", "12345678")
	admin, _ := e.login("admin@trytuist.com", "admin12345")
	const tokA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	const tokB = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	// Daftar device: validasi & idempotent.
	expect(t, e.do("PUT", "/v1/me/devices", token, map[string]any{"token": "bukan-hex", "bundle_id": "x", "environment": "sandbox"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("PUT", "/v1/me/devices", token, map[string]any{"token": tokA, "bundle_id": "com.lshinkuro.try-tuist.dev", "environment": "staging"}), 400, "VALIDATION_ERROR")
	for i := 0; i < 2; i++ {
		expect(t, e.do("PUT", "/v1/me/devices", token, map[string]any{"token": tokA, "bundle_id": "com.lshinkuro.try-tuist.dev", "environment": "sandbox"}), 200, "")
	}
	expect(t, e.do("PUT", "/v1/me/devices", token, map[string]any{"token": tokB, "bundle_id": "com.lshinkuro.try-tuist.dev", "environment": "sandbox"}), 200, "")
	expect(t, e.do("PUT", "/v1/me/devices", "", map[string]any{"token": tokA, "bundle_id": "x", "environment": "sandbox"}), 401, "UNAUTHORIZED")

	// Bayar online → push "Pembayaran berhasil" ke dua device; device B mati → dihapus.
	apns.dead[tokB] = true
	r := e.do("POST", "/v1/checkout", token, map[string]any{"payment_method_id": "ONLINE",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}}, "Idempotency-Key", "push-1")
	expect(t, r, 201, "")
	p := r.data()["payment"].(map[string]any)
	oid := p["provider_order_id"].(string)
	st := gw.set(oid, "settlement", "gopay", 32000)
	e.notify(st, signature(oid, "200", "32000.00"))
	e.app.Push.Wait()
	if got := apns.titles(); len(got) != 1 || got[0] != "Pembayaran berhasil 🎉" {
		t.Fatalf("harap 1 push pembayaran berhasil, dapat %v", got)
	}
	msg := apns.sent[0].Message
	if msg.Data["type"] != "payment_paid" || msg.Data["order_id"] == "" || apns.sent[0].Device.BundleID != "com.lshinkuro.try-tuist.dev" {
		t.Fatalf("isi push salah: %+v", apns.sent[0])
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM push_devices WHERE token = $1`, tokB).Scan(&n)
	if n != 0 {
		t.Fatal("token yang Unregistered harus dihapus")
	}

	// Admin mengubah status → push status pesanan, dengan alasan bila gagal.
	orderID := msg.Data["order_id"]
	expect(t, e.do("PATCH", "/v1/admin/orders/"+orderID+"/status", admin, map[string]any{"status": "PROCESSING"}), 200, "")
	expect(t, e.do("PATCH", "/v1/admin/orders/"+orderID+"/status", admin, map[string]any{"status": "FAILED", "note": "Stok habis"}), 200, "")
	e.app.Push.Wait()
	got := apns.titles()
	if len(got) != 3 || got[1] != "Pesanan sedang disiapkan 👨‍🍳" || got[2] != "Pesanan gagal" {
		t.Fatalf("push status salah: %v", got)
	}
	if body := apns.sent[2].Message.Body; body == "" || body[len(body)-len("Stok habis"):] != "Stok habis" {
		t.Fatalf("alasan gagal harus ikut di push: %q", body)
	}

	// Pembayaran kedaluwarsa (sweeper) → push.
	r = e.do("POST", "/v1/checkout", token, map[string]any{"payment_method_id": "ONLINE",
		"items": []map[string]any{{"product_id": "P01", "quantity": 1}}}, "Idempotency-Key", "push-2")
	p2 := r.data()["payment"].(map[string]any)
	if _, err := e.pool.Exec(ctx, `UPDATE payments SET expires_at = now() - interval '10 minutes' WHERE id = $1`, p2["id"]); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Payments.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	e.app.Push.Wait()
	if got := apns.titles(); got[len(got)-1] != "Waktu pembayaran habis" {
		t.Fatalf("harap push kedaluwarsa: %v", got)
	}

	// Logout di app → unregister tanpa login; setelah itu tidak ada push lagi.
	expect(t, e.do("POST", "/v1/devices/unregister", "", map[string]any{"token": tokA}), 200, "")
	expect(t, e.do("POST", "/v1/devices/unregister", "", map[string]any{"token": tokA}), 200, "") // idempotent
	before := len(apns.titles())
	expect(t, e.do("PATCH", "/v1/admin/orders/"+orderID+"/status", admin, map[string]any{"status": "CANCELLED", "note": "x"}), 422, "INVALID_STATUS_TRANSITION")
	expect(t, e.do("POST", "/v1/dev/push-test", token, nil), 200, "")
	e.app.Push.Wait()
	if len(apns.titles()) != before {
		t.Fatal("device yang sudah unregister tidak boleh menerima push")
	}

	// Token pindah ke user lain yang login di device yang sama.
	demo, _ := e.login("demo@trytuist.com", "password123")
	expect(t, e.do("PUT", "/v1/me/devices", token, map[string]any{"token": tokA, "bundle_id": "b", "environment": "sandbox"}), 200, "")
	expect(t, e.do("PUT", "/v1/me/devices", demo, map[string]any{"token": tokA, "bundle_id": "b", "environment": "sandbox"}), 200, "")
	var owner string
	_ = e.pool.QueryRow(ctx, `SELECT u.email FROM push_devices d JOIN users u ON u.id = d.user_id WHERE d.token = $1`, tokA).Scan(&owner)
	if owner != "demo@trytuist.com" {
		t.Fatalf("token harus pindah ke user terakhir, dapat %s", owner)
	}
}
