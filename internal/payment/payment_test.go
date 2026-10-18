package payment

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	n := Notification{TransactionStatus: TransactionStatus{OrderID: "TT-DEV-1", StatusCode: "200", GrossAmount: "54000.00"}}
	sum := sha512.Sum512([]byte("TT-DEV-1" + "200" + "54000.00" + "SB-key"))
	n.SignatureKey = hex.EncodeToString(sum[:])
	if !VerifySignature("SB-key", n) {
		t.Fatal("signature valid ditolak")
	}
	if VerifySignature("SB-other", n) {
		t.Fatal("signature dengan key lain diterima")
	}
}

func TestTarget(t *testing.T) {
	cases := []struct {
		status, fraud, want, refund string
	}{
		{"settlement", "", StatusPaid, ""},
		{"capture", "accept", StatusPaid, ""},
		{"capture", "challenge", "", ""},
		{"pending", "", "", ""},
		{"deny", "", StatusFailed, ""},
		{"expire", "", StatusExpired, ""},
		{"cancel", "", StatusCancelled, ""},
		{"refund", "", "", "REFUND"},
	}
	for _, c := range cases {
		got, refund := target(TransactionStatus{TransactionStatus: c.status, FraudStatus: c.fraud})
		if got != c.want || refund != c.refund {
			t.Errorf("%s/%s → %q %q, harap %q %q", c.status, c.fraud, got, refund, c.want, c.refund)
		}
	}
	if a, ok := parseAmount("54000.00"); !ok || a != 54000 {
		t.Fatal("parseAmount")
	}
}

func TestHTTPGateway(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/snap/v1/transactions":
			var req SnapRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.TransactionDetails.GrossAmount <= 0 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error_messages":["transaction_details.gross_amount is required"]}`))
				return
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"token":"abc","redirect_url":"https://app.sandbox.midtrans.com/snap/v4/redirection/abc"}`))
		case r.URL.Path == "/v2/TT-NONE/status":
			_, _ = w.Write([]byte(`{"status_code":"404","status_message":"Transaction doesn't exist."}`))
		case r.URL.Path == "/v2/TT-OK/status":
			_, _ = w.Write([]byte(`{"status_code":"200","transaction_status":"settlement","order_id":"TT-OK","gross_amount":"54000.00","payment_type":"gopay"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := NewHTTPGateway("SB-key", false)
	g.snapURL, g.apiBase = srv.URL+"/snap/v1/transactions", srv.URL+"/v2"

	var req SnapRequest
	req.TransactionDetails.OrderID, req.TransactionDetails.GrossAmount = "TT-1", 1000
	res, err := g.CreateSnap(context.Background(), req)
	if err != nil || res.Token != "abc" {
		t.Fatalf("CreateSnap: %v %v", res, err)
	}
	if gotAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("SB-key:")) {
		t.Fatalf("header Authorization salah: %q", gotAuth)
	}
	req.TransactionDetails.GrossAmount = 0
	var ge *GatewayError
	if _, err := g.CreateSnap(context.Background(), req); !errors.As(err, &ge) || ge.HTTPStatus != 400 {
		t.Fatalf("harap GatewayError 400, dapat %v", err)
	}
	if _, err := g.Status(context.Background(), "TT-NONE"); !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("status_code 404 di body harus ErrTransactionNotFound, dapat %v", err)
	}
	if st, err := g.Status(context.Background(), "TT-OK"); err != nil || st.TransactionStatus != "settlement" {
		t.Fatalf("Status: %v %v", st, err)
	}
}
