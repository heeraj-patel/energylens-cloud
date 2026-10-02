package programs

import (
	"slices"
	"testing"
	"time"
)

func TestZIPCandidateDoesNotProveUtilityService(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := Program{ID: "utility-rebate", Status: "open", ZIPs: []string{"78732"}, Category: "washer", Fuel: "electricity", ProviderID: "utility-a", ReviewedAt: now}
	h := Household{ZIP: "78732", Category: "washer", Fuel: "electricity", CandidateProvider: map[string]bool{"utility-a": true}}
	got := Evaluate(p, h, now)
	if got.Status != "unknown" || !slices.Contains(got.Reasons, "provider_unconfirmed") {
		t.Fatalf("candidate service: %+v", got)
	}
	h.ConfirmedProvider = map[string]bool{"utility-a": true}
	got = Evaluate(p, h, now)
	if got.Status != "potentially_eligible" || !slices.Contains(got.Reasons, "terms_require_confirmation") {
		t.Fatalf("confirmed service: %+v", got)
	}
}

func TestExpiredAndUnknownRequirements(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := Program{ID: "state", Status: "open", EndsOn: "2025-12-31", RequiresIncome: true, ReviewedAt: now}
	if got := Evaluate(p, Household{}, now); got.Status != "ineligible" || got.Reasons[0] != "expired" {
		t.Fatalf("expired program: %+v", got)
	}
	p.EndsOn = ""
	if got := Evaluate(p, Household{}, now); got.Status != "unknown" || !slices.Contains(got.Reasons, "income_unknown") {
		t.Fatalf("unknown income: %+v", got)
	}
	p.ReviewedAt = time.Time{}
	if got := Evaluate(p, Household{}, now); got.Status != "unknown" || got.Reasons[0] != "source_unreviewed" {
		t.Fatalf("unreviewed source: %+v", got)
	}
	p.ReviewedAt = now.Add(-time.Hour)
	p.ReviewExpiresAt = now.Add(-time.Minute)
	if got := Evaluate(p, Household{}, now); got.Status != "unknown" || got.Reasons[0] != "source_review_expired" {
		t.Fatalf("expired review: %+v", got)
	}
	p.ReviewExpiresAt = time.Time{}
	p.StartsOn = "2026-02-30"
	if got := Evaluate(p, Household{}, now); got.Status != "unknown" || got.Reasons[0] != "invalid_program_date" {
		t.Fatalf("invalid date: %+v", got)
	}
}
