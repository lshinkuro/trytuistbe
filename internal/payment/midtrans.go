// Package payment: pembayaran online lewat Midtrans Snap (API_PLAN.md §5.27).
package payment

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gateway: tiga panggilan ke Midtrans yang dipakai. Di tes diganti fake.
type Gateway interface {
	CreateSnap(ctx context.Context, req SnapRequest) (SnapResponse, error)
	// Status: ErrTransactionNotFound bila user belum memilih metode bayar di Snap.
	Status(ctx context.Context, orderID string) (TransactionStatus, error)
	// Expire: hentikan transaksi pending (VA/QR tidak bisa dibayar lagi).
	Expire(ctx context.Context, orderID string) error
	// Refund: kembalikan dana (penuh / sebagian). refund_key yang sama tidak diproses dua kali oleh Midtrans.
	Refund(ctx context.Context, orderID string, req RefundRequest) error
	// VerifySignature: signature_key notifikasi = SHA512(order_id + status_code + gross_amount + server key).
	VerifySignature(n Notification) bool
}

var ErrTransactionNotFound = errors.New("midtrans: transaksi belum ada")

type SnapItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Quantity int    `json:"quantity"`
}

type SnapCustomer struct {
	FirstName string `json:"first_name,omitempty"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
}

type SnapExpiry struct {
	StartTime string `json:"start_time"` // "2026-10-18 15:40:00 +0700"
	Unit      string `json:"unit"`
	Duration  int    `json:"duration"`
}

type SnapRequest struct {
	TransactionDetails struct {
		OrderID     string `json:"order_id"`
		GrossAmount int64  `json:"gross_amount"`
	} `json:"transaction_details"`
	ItemDetails     []SnapItem    `json:"item_details,omitempty"`
	CustomerDetails *SnapCustomer `json:"customer_details,omitempty"`
	Expiry          *SnapExpiry   `json:"expiry,omitempty"`
	Callbacks       *struct {
		Finish string `json:"finish"`
	} `json:"callbacks,omitempty"`
	EnabledPayments []string `json:"enabled_payments,omitempty"`
	CustomField1    string   `json:"custom_field1,omitempty"`
}

type SnapResponse struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirect_url"`
}

// TransactionStatus: bagian dari response GET Status / body notifikasi yang dipakai.
type TransactionStatus struct {
	OrderID           string `json:"order_id"`
	TransactionID     string `json:"transaction_id"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	PaymentType       string `json:"payment_type"`
	StatusCode        string `json:"status_code"`
	StatusMessage     string `json:"status_message"`
	GrossAmount       string `json:"gross_amount"` // "49800.00"
}

type RefundRequest struct {
	RefundKey string `json:"refund_key"`
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason,omitempty"`
}

// Notification: body webhook Midtrans (field lain diabaikan).
type Notification struct {
	TransactionStatus
	SignatureKey string `json:"signature_key"`
}

// ===== Klien HTTP =====

type HTTPGateway struct {
	serverKey string
	snapURL   string // …/snap/v1/transactions
	apiBase   string // …/v2
	client    *http.Client
}

func NewHTTPGateway(serverKey string, production bool) *HTTPGateway {
	g := &HTTPGateway{
		serverKey: serverKey,
		snapURL:   "https://app.sandbox.midtrans.com/snap/v1/transactions",
		apiBase:   "https://api.sandbox.midtrans.com/v2",
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	if production {
		g.snapURL = "https://app.midtrans.com/snap/v1/transactions"
		g.apiBase = "https://api.midtrans.com/v2"
	}
	return g
}

// GatewayError: Midtrans menolak permintaan (dicatat apa adanya, tidak ditampilkan ke user).
type GatewayError struct {
	HTTPStatus int
	Messages   string
}

func (e *GatewayError) Error() string {
	return fmt.Sprintf("midtrans: HTTP %d: %s", e.HTTPStatus, e.Messages)
}

func (g *HTTPGateway) do(ctx context.Context, method, rawURL string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(g.serverKey+":")))
	res, err := g.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return b, res.StatusCode, err
}

func (g *HTTPGateway) CreateSnap(ctx context.Context, req SnapRequest) (SnapResponse, error) {
	b, status, err := g.do(ctx, http.MethodPost, g.snapURL, req)
	if err != nil {
		return SnapResponse{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		var e struct {
			ErrorMessages []string `json:"error_messages"`
		}
		_ = json.Unmarshal(b, &e)
		return SnapResponse{}, &GatewayError{HTTPStatus: status, Messages: strings.Join(e.ErrorMessages, "; ")}
	}
	var out SnapResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return SnapResponse{}, err
	}
	if out.Token == "" || out.RedirectURL == "" {
		return SnapResponse{}, &GatewayError{HTTPStatus: status, Messages: "token / redirect_url kosong"}
	}
	return out, nil
}

func (g *HTTPGateway) Status(ctx context.Context, orderID string) (TransactionStatus, error) {
	b, status, err := g.do(ctx, http.MethodGet, g.apiBase+"/"+url.PathEscape(orderID)+"/status", nil)
	if err != nil {
		return TransactionStatus{}, err
	}
	var out TransactionStatus
	if err := json.Unmarshal(b, &out); err != nil {
		return TransactionStatus{}, fmt.Errorf("midtrans status: HTTP %d: %w", status, err)
	}
	// Core API membalas HTTP 200 dengan status_code "404" bila transaksi belum ada.
	if status == http.StatusNotFound || out.StatusCode == "404" {
		return TransactionStatus{}, ErrTransactionNotFound
	}
	if status >= 300 || out.TransactionStatus == "" {
		return TransactionStatus{}, &GatewayError{HTTPStatus: status, Messages: out.StatusCode + " " + out.StatusMessage}
	}
	return out, nil
}

func (g *HTTPGateway) Expire(ctx context.Context, orderID string) error {
	b, status, err := g.do(ctx, http.MethodPost, g.apiBase+"/"+url.PathEscape(orderID)+"/expire", nil)
	if err != nil {
		return err
	}
	var out TransactionStatus
	_ = json.Unmarshal(b, &out)
	if status == http.StatusNotFound || out.StatusCode == "404" {
		return ErrTransactionNotFound
	}
	if status >= 300 || (out.StatusCode != "" && out.StatusCode != "200" && out.StatusCode != "407") {
		return &GatewayError{HTTPStatus: status, Messages: out.StatusCode + " " + out.StatusMessage}
	}
	return nil
}

func (g *HTTPGateway) Refund(ctx context.Context, orderID string, req RefundRequest) error {
	b, status, err := g.do(ctx, http.MethodPost, g.apiBase+"/"+url.PathEscape(orderID)+"/refund", req)
	if err != nil {
		return err
	}
	var out TransactionStatus
	_ = json.Unmarshal(b, &out)
	// Berhasil = status_code "200"; selain itu (412 status tidak bisa diubah, 413 jumlah salah, …) ditolak.
	if status >= 300 || out.StatusCode != "200" {
		return &GatewayError{HTTPStatus: status, Messages: strings.TrimSpace(out.StatusCode + " " + out.StatusMessage)}
	}
	return nil
}

func (g *HTTPGateway) VerifySignature(n Notification) bool {
	return VerifySignature(g.serverKey, n)
}

// VerifySignature: SHA512(order_id + status_code + gross_amount + serverKey), hex, constant-time.
func VerifySignature(serverKey string, n Notification) bool {
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + serverKey))
	want := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(n.SignatureKey))) == 1
}
