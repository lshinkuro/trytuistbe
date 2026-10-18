package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestAPNsSend(t *testing.T) {
	key, p8 := testKey(t)
	var got struct {
		path, auth, topic, pushType string
		body                        map[string]any
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("authorization")
		got.topic, got.pushType = r.Header.Get("apns-topic"), r.Header.Get("apns-push-type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/dead"):
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
		case strings.HasSuffix(r.URL.Path, "/badtopic"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"DeviceTokenNotForTopic"}`))
		case strings.HasSuffix(r.URL.Path, "/badkey"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"reason":"InvalidProviderToken"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	a, err := NewAPNs("KEY123", "TEAM456", p8)
	if err != nil {
		t.Fatal(err)
	}
	a.client = srv.Client()
	a.sandboxURL, a.productionURL = srv.URL, srv.URL

	err = a.Send(context.Background(), Device{Token: "abcd", BundleID: "com.lshinkuro.try-tuist.dev", Environment: "sandbox"},
		Message{Title: "Pembayaran berhasil 🎉", Body: "ORD-1", ThreadID: "o1", Data: map[string]string{"type": "payment_paid", "order_id": "o1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/3/device/abcd" || got.topic != "com.lshinkuro.try-tuist.dev" || got.pushType != "alert" {
		t.Fatalf("request APNs salah: %+v", got)
	}
	jwt := strings.TrimPrefix(got.auth, "bearer ")
	if !verifyES256(&key.PublicKey, jwt) {
		t.Fatal("JWT provider token tidak valid")
	}
	aps := got.body["aps"].(map[string]any)
	alert := aps["alert"].(map[string]any)
	if alert["title"] != "Pembayaran berhasil 🎉" || aps["thread-id"] != "o1" || got.body["type"] != "payment_paid" || got.body["order_id"] != "o1" {
		t.Fatalf("payload salah: %v", got.body)
	}

	// Token di-cache.
	first := jwt
	_ = a.Send(context.Background(), Device{Token: "abcd", BundleID: "x"}, Message{Title: "x"})
	if strings.TrimPrefix(got.auth, "bearer ") != first {
		t.Fatal("provider token harus di-cache")
	}

	if err := a.Send(context.Background(), Device{Token: "dead", BundleID: "x"}, Message{}); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("410 harus ErrUnregistered, dapat %v", err)
	}
	if err := a.Send(context.Background(), Device{Token: "badtopic", BundleID: "x"}, Message{}); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("DeviceTokenNotForTopic harus ErrUnregistered, dapat %v", err)
	}
	var ae *APNsError
	if err := a.Send(context.Background(), Device{Token: "badkey", BundleID: "x"}, Message{}); !errors.As(err, &ae) || ae.Reason != "InvalidProviderToken" {
		t.Fatalf("harap APNsError InvalidProviderToken, dapat %v", err)
	}
}

func TestNewAPNsRejectsBadKey(t *testing.T) {
	if _, err := NewAPNs("K", "T", []byte("bukan pem")); err == nil {
		t.Fatal("harus error")
	}
	_, p8 := testKey(t)
	if _, err := NewAPNs("", "T", p8); err == nil {
		t.Fatal("key id kosong harus error")
	}
}
