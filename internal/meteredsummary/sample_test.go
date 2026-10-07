package meteredsummary

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSampleProviderPreservesFixtureDateAndUnknownEnergy(t *testing.T) {
	user, home := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	s, err := NewSampleProvider(map[string]bool{user: true}, map[string]bool{home: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	q := Query{OwnerID: user, HomeID: home, Granularity: "day", From: start, To: start.Add(24 * time.Hour)}
	if !s.allowed(q) || len(s.page(q)) != 1 {
		t.Fatal("allowed fixture day missing")
	}
	var row map[string]any
	if err := json.Unmarshal(s.page(q)[0], &row); err != nil {
		t.Fatal(err)
	}
	if row["status"] != "no_intervals" || row["energy_wh"] != nil || row["covered_seconds"] != nil || row["bucket_start"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("fixture fabricated evidence: %+v", row)
	}
	q.From, q.To = start.Add(24*time.Hour), start.Add(48*time.Hour)
	if len(s.page(q)) != 0 {
		t.Fatal("sample escaped requested range")
	}
	q.From, q.To, q.Granularity = start, start.Add(24*time.Hour), "hour"
	if len(s.page(q)) != 0 {
		t.Fatal("daily sample appeared in hourly query")
	}
	q.OwnerID = "33333333-3333-4333-8333-333333333333"
	if s.allowed(q) {
		t.Fatal("unlisted user enabled sample")
	}
	q.OwnerID, q.HomeID = user, "44444444-4444-4444-8444-444444444444"
	if s.allowed(q) {
		t.Fatal("unlisted home enabled sample")
	}
}
