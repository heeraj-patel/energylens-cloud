package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenMeteoProviderValidatesResponseAndCoarseLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("apikey") != "paid-key" || q.Get("latitude") != "41.5000" || q.Get("longitude") != "-87.5000" || q.Get("timezone") != "America/Chicago" || q.Get("start_date") != "2026-09-30" || q.Get("daily") != "temperature_2m_mean" {
			t.Errorf("request parameters: %s", r.URL.Redacted())
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"timezone":"America/Chicago","daily":{"time":["2026-09-30"],"temperature_2m_mean":[18.5]},"daily_units":{"temperature_2m_mean":"°C"}}`))
	}))
	defer server.Close()
	p := OpenMeteo{APIKey: "paid-key", Cells: map[string]Coordinate{"US:606:America/Chicago": {Latitude: 41.5, Longitude: -87.5}}, Endpoint: server.URL}
	mean, rev, err := p.FetchDaily(context.Background(), "US:606:America/Chicago", "America/Chicago", "2026-09-30")
	if err != nil || mean != 18.5 || rev == "" {
		t.Fatalf("weather: %v %v %v", mean, rev, err)
	}
	if _, _, err = p.FetchDaily(context.Background(), "US:605:America/Chicago", "America/Chicago", "2026-09-30"); err == nil {
		t.Fatal("unmapped cell accepted")
	}
	p.APIKey = ""
	if _, _, err = p.FetchDaily(context.Background(), "US:606:America/Chicago", "America/Chicago", "2026-09-30"); err == nil {
		t.Fatal("missing license credential accepted")
	}
}
func TestOpenMeteoProviderErrorsFailClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad") {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"timezone":"UTC","daily":{"time":["2026-09-30"],"temperature_2m_mean":[null]},"daily_units":{"temperature_2m_mean":"°C"}}`))
	}))
	defer server.Close()
	p := OpenMeteo{APIKey: "key", Cells: map[string]Coordinate{"US:606:UTC": {Latitude: 41.5, Longitude: -87.5}}, Endpoint: server.URL}
	if _, _, err := p.FetchDaily(context.Background(), "US:606:UTC", "UTC", "2026-09-30"); err == nil {
		t.Fatal("null temperature accepted")
	}
	p.Endpoint = server.URL + "/bad"
	if _, _, err := p.FetchDaily(context.Background(), "US:606:UTC", "UTC", "2026-09-30"); err == nil {
		t.Fatal("provider failure accepted")
	}
}
func TestParseCoarseCells(t *testing.T) {
	if _, err := ParseCoarseCells(`{"US:606:America/Chicago":{"latitude":41.5,"longitude":-87.5}}`); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"US:60601:America/Chicago":{"latitude":41.5,"longitude":-87.5}}`, `{"US:606:America/Chicago":{"latitude":999,"longitude":0}}`} {
		if _, err := ParseCoarseCells(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
