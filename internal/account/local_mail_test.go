package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResendLocalSenderUsesFixedFragmentLinksAndNoSecretHeaders(t *testing.T) {
	requests := 0
	var payload struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
		HTML    string   `json:"html"`
	}
	var idem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/emails" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("unexpected auth mail request")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil || json.Unmarshal(body, &payload) != nil {
			t.Error("invalid payload")
		}
		idem = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test-receipt"}`))
	}))
	defer server.Close()
	sender, err := NewResendLocalSender("test-key", "auth@example.test", "https://account.example.test")
	if err != nil {
		t.Fatal(err)
	}
	sender.endpoint = server.URL + "/emails"
	first, _, err := NewLocalOpaqueToken("verify_email")
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.SendVerification(context.Background(), "person@example.test", first); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || payload.From != "auth@example.test" || len(payload.To) != 1 || payload.To[0] != "person@example.test" || payload.Subject != "Verify your EnergyLens email" || payload.HTML != "" || !strings.Contains(payload.Text, "https://account.example.test/verify-email#token="+first) || strings.Contains(idem, first) {
		t.Fatal("verification mail violated fixed text/fragment/idempotency contract")
	}
	if !strings.HasPrefix(idem, "auth/verify_email/") {
		t.Fatal("verification idempotency key missing purpose")
	}
	reset, _, err := NewLocalOpaqueToken("reset_password")
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.SendRecovery(context.Background(), "person@example.test", reset); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || payload.Subject != "Reset your EnergyLens password" || !strings.Contains(payload.Text, "https://account.example.test/reset-password#token="+reset) || !strings.HasPrefix(idem, "auth/reset_password/") {
		t.Fatal("reset mail violated purpose separation")
	}
}

func TestResendLocalSenderFailsClosedWithoutLeakingSecrets(t *testing.T) {
	for _, input := range []struct{ key, from, origin string }{
		{"", "auth@example.test", "https://account.example.test"},
		{"bad\nkey", "auth@example.test", "https://account.example.test"},
		{"key", "bad sender", "https://account.example.test"},
		{"key", "auth@example.test", "http://account.example.test"},
		{"key", "auth@example.test", "https://account.example.test/path"},
		{"key", "auth@example.test", "https://account.example.test?token=bad"},
		{"key", "auth@example.test", "https://user@account.example.test"},
	} {
		if _, err := NewResendLocalSender(input.key, input.from, input.origin); err == nil {
			t.Fatalf("accepted unsafe sender config: %+v", input)
		}
	}
	sender, err := NewResendLocalSender("key", "auth@example.test", "https://account.example.test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := NewLocalOpaqueToken("verify_email")
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.SendVerification(context.Background(), "Person@example.test", token); err == nil {
		t.Fatal("noncanonical recipient accepted")
	}
	if err = sender.SendVerification(context.Background(), "person@example.test", token+"x"); err == nil {
		t.Fatal("malformed token accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example.test", http.StatusFound)
	}))
	defer server.Close()
	sender.endpoint = server.URL
	if err = sender.SendVerification(context.Background(), "person@example.test", token); err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "person@example.test") {
		t.Fatal("redirect accepted or secret leaked")
	}
	server.Close()
	if err = sender.SendVerification(context.Background(), "person@example.test", token); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("transport error leaked token")
	}
}
