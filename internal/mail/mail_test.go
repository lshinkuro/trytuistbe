package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResendSend(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["to"].([]any)[0] == "bad@example.com" {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"message":"invalid to"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"em_1"}`))
	}))
	defer srv.Close()
	r := NewResend("re_test", "TryTuist <noreply@trytuist.id>")
	r.baseURL = srv.URL

	if err := r.Send(context.Background(), CodeEmail("a@example.com", "Kholis", "VERIFY_EMAIL", "123456", 15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_test" || got["from"] != "TryTuist <noreply@trytuist.id>" || !strings.Contains(got["text"].(string), "123456") {
		t.Fatalf("request Resend salah: auth=%q body=%v", auth, got)
	}
	if err := r.Send(context.Background(), Message{To: "bad@example.com"}); err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("error Resend harus diteruskan, dapat %v", err)
	}
}

func TestCodeEmailEscapesName(t *testing.T) {
	m := CodeEmail("a@example.com", "<script>", "RESET_PASSWORD", "654321", 15*time.Minute)
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.Subject, "reset password") {
		t.Fatalf("email reset salah: %+v", m)
	}
}
