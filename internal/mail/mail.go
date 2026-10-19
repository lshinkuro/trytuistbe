// Package mail: kirim email transaksional (kode verifikasi & reset password) lewat Resend.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender: Resend di staging/prod, LogSender di dev (tanpa RESEND_API_KEY). Di tes diganti fake.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Resend: POST https://api.resend.com/emails (https://resend.com/docs/api-reference/emails/send-email).
type Resend struct {
	apiKey  string
	from    string
	baseURL string
	client  *http.Client
}

func NewResend(apiKey, from string) *Resend {
	return &Resend{apiKey: apiKey, from: from, baseURL: "https://api.resend.com", client: &http.Client{Timeout: 10 * time.Second}}
}

func (r *Resend) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]any{"from": r.from, "to": []string{m.To}, "subject": m.Subject, "text": m.Text, "html": m.HTML})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("resend: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// LogSender: dev tanpa RESEND_API_KEY. Isi email (termasuk kode) ditulis ke log supaya alur bisa dicoba.
type LogSender struct{}

func (LogSender) Send(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "email (RESEND_API_KEY kosong, tidak dikirim)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// CodeEmail: email berisi kode 6 digit.
func CodeEmail(to, name, purpose, code string, ttl time.Duration) Message {
	subject, intro := "Kode verifikasi email TryTuist", "Masukkan kode ini di aplikasi untuk memverifikasi email kamu:"
	if purpose == "RESET_PASSWORD" {
		subject, intro = "Kode reset password TryTuist", "Ada permintaan reset password untuk akun TryTuist kamu. Masukkan kode ini untuk membuat password baru:"
	}
	minutes := int(ttl.Minutes())
	text := fmt.Sprintf("Halo %s,\n\n%s\n\n%s\n\nKode berlaku %d menit. Jangan bagikan kode ini kepada siapa pun, termasuk yang mengaku dari TryTuist.\nAbaikan email ini bila kamu tidak memintanya.\n", name, intro, code, minutes)
	html := fmt.Sprintf(`<div style="font-family:system-ui,sans-serif;max-width:480px;margin:auto;color:#1c1f24">
<p>Halo %s,</p><p>%s</p>
<p style="font-size:32px;font-weight:800;letter-spacing:8px;margin:24px 0">%s</p>
<p>Kode berlaku %d menit. Jangan bagikan kode ini kepada siapa pun, termasuk yang mengaku dari TryTuist.</p>
<p style="color:#6b7280;font-size:13px">Abaikan email ini bila kamu tidak memintanya.</p></div>`,
		htmlEscape(name), intro, code, minutes)
	return Message{To: to, Subject: subject, Text: text, HTML: html}
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
