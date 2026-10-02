package reports

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestResendProviderUsesStableIdempotentPayload(t *testing.T) {
	var payloads []string
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		payloads = append(payloads, string(body))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"provider-message-id"}`)
	}))
	defer server.Close()
	provider, err := NewResendProvider("test-key", "reports@example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider.endpoint = server.URL + "/emails"
	report := RenderedReport{Subject: "EnergyLens report", HTML: "<main>Approved snapshot</main>", SnapshotSHA256: strings.Repeat("a", 64)}
	id := uuid.NewString()
	for range 2 {
		if err = provider.SendReport(context.Background(), id, []string{"Z@example.test", "a@example.test"}, report); err != nil {
			t.Fatal(err)
		}
	}
	if len(payloads) != 2 || payloads[0] != payloads[1] || keys[0] != "report/"+id || keys[1] != keys[0] {
		t.Fatalf("retry changed payload or key: keys=%v", keys)
	}
	var sent struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}
	if err = json.Unmarshal([]byte(payloads[0]), &sent); err != nil || sent.From != "reports@example.test" || len(sent.To) != 2 || sent.To[0] != "a@example.test" || sent.To[1] != "z@example.test" || sent.Subject != report.Subject || sent.HTML != report.HTML {
		t.Fatalf("unexpected provider JSON: %+v %v", sent, err)
	}
}

func TestResendProviderFailsClosedOnErrorsAndRedirects(t *testing.T) {
	if _, err := NewResendProvider("", "reports@example.test"); err == nil {
		t.Fatal("missing provider key accepted")
	}
	if _, err := NewResendProvider("key", "bad sender"); err == nil {
		t.Fatal("bad sender accepted")
	}
	redirected := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected = true; w.WriteHeader(200) }))
	defer destination.Close()
	status := http.StatusConflict
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == http.StatusFound {
			w.Header().Set("Location", destination.URL)
		}
		w.WriteHeader(status)
		if status != http.StatusFound {
			_, _ = io.WriteString(w, "recipient@example.test private report body")
		}
	}))
	defer server.Close()
	provider, err := NewResendProvider("test-key", "reports@example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider.endpoint = server.URL
	report := RenderedReport{Subject: "EnergyLens report", HTML: "<main>Report</main>", SnapshotSHA256: strings.Repeat("a", 64)}
	if err = provider.SendReport(context.Background(), uuid.NewString(), []string{"recipient@example.test"}, report); err == nil || strings.Contains(err.Error(), "recipient@example.test") || strings.Contains(err.Error(), "private report body") {
		t.Fatalf("provider error leaked response: %v", err)
	}
	status = http.StatusFound
	if err = provider.SendReport(context.Background(), uuid.NewString(), []string{"recipient@example.test"}, report); err == nil || redirected {
		t.Fatalf("provider redirect followed: %v", err)
	}
	status = http.StatusOK
	if err = provider.SendReport(context.Background(), uuid.NewString(), []string{"recipient@example.test"}, report); err == nil {
		t.Fatal("provider response without receipt accepted")
	}
}
