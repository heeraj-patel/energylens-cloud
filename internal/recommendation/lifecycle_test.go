package recommendation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestRecommendationLifecycleAndRanking(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	makeAction := func(device string, frequency float64) Action {
		return Action{RuleID: "computer.sleep_settings", RuleVersion: 1, ApplianceID: device, State: "active", BenefitGroup: "computer_idle", EvidenceRefs: []Evidence{{ID: "source", Kind: "annual_energy", Revision: 1}}, usageFrequency: frequency, usageUnit: "hours_per_week"}
	}
	ranked := rankActions([]Action{makeAction("a", 2), makeAction("b", 20)})
	if ranked[0].ApplianceID != "b" || ranked[0].SavingsAmount != nil {
		t.Fatalf("household ranking: %+v", ranked)
	}
	until := now.Add(time.Hour)
	cases := []struct {
		event  LifecycleEvent
		active string
		want   string
	}{
		{LifecycleEvent{RuleVersion: 1, State: "accepted"}, "active", "accepted"},
		{LifecycleEvent{RuleVersion: 1, State: "snoozed", SnoozedUntil: &until}, "active", "snoozed"},
		{LifecycleEvent{RuleVersion: 1, State: "dismissed"}, "active", "dismissed"},
		{LifecycleEvent{RuleVersion: 1, State: "completed"}, "active", "completed"},
		{LifecycleEvent{RuleVersion: 1, State: "dismissed"}, "candidate", "dismissed"},
		{LifecycleEvent{RuleVersion: 1, State: "accepted"}, "candidate", "expired"},
		{LifecycleEvent{RuleVersion: 2, State: "accepted"}, "active", "superseded"},
	}
	for _, tc := range cases {
		a := makeAction("a", 2)
		a.State = tc.active
		got := ApplyLifecycle([]Action{a}, map[string]LifecycleEvent{lifecycleKey("a", a.RuleID): tc.event}, now)
		if got[0].State != tc.want || got[0].SavingsAmount != nil {
			t.Fatalf("%+v: %+v", tc, got)
		}
	}
	if got := ApplyLifecycle([]Action{makeAction("a", 2)}, map[string]LifecycleEvent{lifecycleKey("a", "computer.sleep_settings"): {RuleVersion: 1, State: "snoozed", SnoozedUntil: &until}}, until)[0]; got.State != "active" {
		t.Fatalf("snooze expiry: %+v", got)
	}
	for _, transition := range []struct {
		from, to string
		valid    bool
	}{{"", "accepted", true}, {"accepted", "completed", true}, {"", "completed", false}, {"completed", "accepted", false}, {"dismissed", "accepted", true}} {
		if validTransition(transition.from, transition.to) != transition.valid {
			t.Fatalf("transition %+v", transition)
		}
	}
}

type lifecycleStub struct {
	*stubStore
	events map[string]LifecycleEvent
	saved  map[string]LifecycleEvent
}

func (s *lifecycleStub) Latest(_ context.Context, _, _ string) (map[string]LifecycleEvent, error) {
	return s.events, nil
}
func (s *lifecycleStub) Replay(_ context.Context, _, _, key string) (*LifecycleEvent, error) {
	if e, ok := s.saved[key]; ok {
		return &e, nil
	}
	return nil, nil
}
func (s *lifecycleStub) Record(_ context.Context, _, _, key string, e LifecycleEvent) (LifecycleEvent, error) {
	s.saved[key] = e
	s.events[lifecycleKey(e.ApplianceID, e.RuleID)] = e
	return e, nil
}

func TestRecommendationEventRouteAndStaleRetry(t *testing.T) {
	home, device, key := uuid.NewString(), uuid.NewString(), uuid.NewString()
	store := &lifecycleStub{stubStore: &stubStore{appliances: []Appliance{{ID: device, Category: "refrigerator", Fuel: "electricity", Identity: "confirmed_exact", Facts: []Evidence{{ID: uuid.NewString(), Kind: "annual_energy", Revision: 1}}, Tariff: &Evidence{ID: uuid.NewString(), Kind: "tariff", Revision: 1}}}}, events: map[string]LifecycleEvent{}, saved: map[string]LifecycleEvent{}}
	router := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(router)
	payload := `{"appliance_id":"` + device + `","rule_id":"refrigerator.maintenance","rule_version":1,"state":"accepted"}`
	post := func(k, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/homes/"+home+"/recommendation-events", strings.NewReader(body))
		r.Header.Set("Idempotency-Key", k)
		router.ServeHTTP(w, r)
		return w.Code
	}
	if code := post(key, payload); code != http.StatusOK {
		t.Fatalf("first post: %d", code)
	}
	if store.owner != "owner" || store.home != home {
		t.Fatalf("owner scope: %+v", store.stubStore)
	}
	store.appliances = nil // An uncertain retry still returns its stored event.
	if code := post(key, payload); code != http.StatusOK {
		t.Fatalf("stale retry: %d", code)
	}
	if code := post(key, strings.Replace(payload, "accepted", "dismissed", 1)); code != http.StatusConflict {
		t.Fatalf("key reuse: %d", code)
	}
	if code := post(uuid.NewString(), payload); code != http.StatusConflict {
		t.Fatalf("stale new action: %d", code)
	}
	if code := post("bad", payload); code != http.StatusBadRequest {
		t.Fatalf("invalid key: %d", code)
	}
}
