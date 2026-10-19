package server_test

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"trytuist-api/internal/config"
	"trytuist-api/internal/mail"
	"trytuist-api/internal/server"
)

// fakeMail: pengganti Resend; menyimpan email untuk mengambil kode.
type fakeMail struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (f *fakeMail) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

var codeRe = regexp.MustCompile(`\b\d{6}\b`)

func (f *fakeMail) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// lastCode: kode di email terakhir untuk alamat ini.
func (f *fakeMail) lastCode(t *testing.T, to string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].To == to {
			return codeRe.FindString(f.sent[i].Text)
		}
	}
	t.Fatalf("tidak ada email ke %s", to)
	return ""
}

func TestEmailVerificationAndPasswords(t *testing.T) {
	mails := &fakeMail{}
	e := setupWith(t, func(c *config.Config) { c.RequireVerifiedEmail = true }, server.WithMailSender(mails))
	ctx := context.Background()
	order := map[string]any{"store_id": "ST01", "payment_method_id": "COD", "items": []map[string]any{{"product_id": "P01", "quantity": 1}}}

	// ===== Verifikasi email =====
	reg := e.do("POST", "/v1/auth/register", "", map[string]any{"email": "baru@example.com", "password": "rahasia123", "name": "Baru"})
	expect(t, reg, 201, "")
	token := reg.data()["access_token"].(string)
	if reg.data()["email_verified"] != false || e.do("GET", "/v1/me", token, nil).data()["email_verified"] != false {
		t.Fatal("akun baru belum terverifikasi")
	}
	first := mails.lastCode(t, "baru@example.com")
	expect(t, e.do("POST", "/v1/checkout", token, order), 403, "EMAIL_NOT_VERIFIED")
	expect(t, e.do("POST", "/v1/me/email/verification", token, nil), 429, "RATE_LIMITED") // cooldown 60 detik

	// 5 kode salah → kode hangus, kode benar pun ditolak.
	wrong := "000000"
	if first == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		expect(t, e.do("POST", "/v1/me/email/verify", token, map[string]any{"code": wrong}), 422, "INVALID_CODE")
	}
	expect(t, e.do("POST", "/v1/me/email/verify", token, map[string]any{"code": first}), 422, "CODE_LOCKED")

	// Minta kode baru (lewati cooldown) → kode lama tidak berlaku, kode baru memverifikasi.
	if _, err := e.pool.Exec(ctx, `UPDATE email_codes SET created_at = now() - interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("POST", "/v1/me/email/verification", token, nil), 200, "")
	second := mails.lastCode(t, "baru@example.com")
	if second != first {
		expect(t, e.do("POST", "/v1/me/email/verify", token, map[string]any{"code": first}), 422, "INVALID_CODE")
	}
	expect(t, e.do("POST", "/v1/me/email/verify", token, map[string]any{"code": second}), 200, "")
	if e.do("GET", "/v1/me", token, nil).data()["email_verified"] != true {
		t.Fatal("email harus terverifikasi")
	}
	expect(t, e.do("POST", "/v1/me/email/verify", token, map[string]any{"code": second}), 409, "EMAIL_ALREADY_VERIFIED")
	expect(t, e.do("POST", "/v1/checkout", token, order), 201, "")

	// ===== Lupa password =====
	before := mails.count()
	expect(t, e.do("POST", "/v1/auth/password/forgot", "", map[string]any{"email": "tidakada@example.com"}), 200, "")
	if mails.count() != before {
		t.Fatal("email tidak terdaftar tidak boleh dikirimi apa pun (respons tetap 200)")
	}
	_, oldRefresh := e.login("kholis@gmail.com", "12345678")
	expect(t, e.do("POST", "/v1/auth/password/forgot", "", map[string]any{"email": "Kholis@Gmail.com"}), 200, "")
	reset := mails.lastCode(t, "kholis@gmail.com")
	expect(t, e.do("POST", "/v1/auth/password/reset", "", map[string]any{"email": "kholis@gmail.com", "code": "999999x", "new_password": "baru12345"}), 400, "VALIDATION_ERROR")
	bad := "123456"
	if reset == bad {
		bad = "654321"
	}
	expect(t, e.do("POST", "/v1/auth/password/reset", "", map[string]any{"email": "kholis@gmail.com", "code": bad, "new_password": "baru12345"}), 422, "INVALID_CODE")
	ok := e.do("POST", "/v1/auth/password/reset", "", map[string]any{"email": "kholis@gmail.com", "code": reset, "new_password": "baru12345"})
	expect(t, ok, 200, "")
	if ok.data()["access_token"] == nil {
		t.Fatal("reset password langsung memberi sesi")
	}
	expect(t, e.do("POST", "/v1/auth/password/reset", "", map[string]any{"email": "kholis@gmail.com", "code": reset, "new_password": "lagi12345"}), 422, "INVALID_CODE") // sekali pakai
	expect(t, e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": oldRefresh}), 401, "UNAUTHORIZED")                                                    // sesi lama dicabut
	expect(t, e.do("POST", "/v1/auth/login", "", map[string]any{"email": "kholis@gmail.com", "password": "12345678"}), 401, "INVALID_CREDENTIALS")
	token, refresh := e.login("kholis@gmail.com", "baru12345")

	// ===== Ganti password =====
	expect(t, e.do("POST", "/v1/me/password", token, map[string]any{"current_password": "salah123", "new_password": "ganti12345"}), 400, "VALIDATION_ERROR")
	expect(t, e.do("POST", "/v1/me/password", token, map[string]any{"current_password": "baru12345", "new_password": "baru12345"}), 400, "VALIDATION_ERROR")
	ch := e.do("POST", "/v1/me/password", token, map[string]any{"current_password": "baru12345", "new_password": "ganti12345"})
	expect(t, ch, 200, "")
	if ch.data()["refresh_token"] == nil {
		t.Fatal("ganti password memberi sesi baru untuk perangkat ini")
	}
	expect(t, e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh}), 401, "UNAUTHORIZED") // perangkat lain keluar
	e.login("kholis@gmail.com", "ganti12345")
}
