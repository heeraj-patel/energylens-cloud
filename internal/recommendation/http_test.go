package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type stubStore struct {
	appliances  []Appliance
	err         error
	owner, home string
}

func (s *stubStore) Current(_ context.Context, owner, home string, _ time.Time) ([]Appliance, error) {
	s.owner, s.home = owner, home
	return s.appliances, s.err
}

func TestRecommendationsRouteUsesOwnerAndNeverPromisesSavings(t *testing.T) {
	home, device := uuid.NewString(), uuid.NewString()
	store := &stubStore{appliances: []Appliance{{ID: device, Category: "refrigerator", Fuel: "electricity", Identity: "confirmed_exact",
		Facts:  []Evidence{{ID: uuid.NewString(), Kind: "annual_energy", Revision: 1}},
		Tariff: &Evidence{ID: uuid.NewString(), Kind: "tariff", Revision: 2}}}}
	r := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusOK || store.owner != "owner" || store.home != home {
		t.Fatalf("response %d, owner %q, home %q", w.Code, store.owner, store.home)
	}
	var body struct {
		Actions []Action `json:"actions"`
		AsOf    string   `json:"as_of"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Actions) != 1 || body.Actions[0].State != "active" || len(body.Actions[0].EvidenceRefs) != 2 || body.Actions[0].SavingsAmount != nil || body.AsOf == "" {
		t.Fatalf("unbounded action: %+v", body)
	}
	store.err = ErrNotFound
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign home: %d", w.Code)
	}
	store.err = errors.New("storage unavailable")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure: %d", w.Code)
	}
}

func TestGasRuleRequiresExplicitRequestCapability(t *testing.T) {
	home := uuid.NewString()
	store := &stubStore{appliances: []Appliance{{ID: uuid.NewString(), Category: "dryer", Fuel: "natural_gas", Identity: "confirmed_exact",
		GasUsage:  &Evidence{ID: uuid.NewString(), Kind: "gas_cycle_energy", Revision: 1},
		Answers:   map[string]Evidence{"cycles_per_week": {ID: uuid.NewString(), Kind: "confirmed_answer", Revision: 1}},
		GasTariff: &Evidence{ID: uuid.NewString(), Kind: "gas_tariff", Revision: 1}}}}
	r := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	request := func(query string) Action {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations"+query, nil))
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("response: %d %v", w.Code, w.Header())
		}
		var body struct {
			Actions []Action `json:"actions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Actions) != 1 {
			t.Fatalf("actions: %+v", body.Actions)
		}
		return body.Actions[0]
	}
	for _, query := range []string{"", "?capability=unknown", "?capability=gas_dryer_recommendations_v0"} {
		if action := request(query); action.RuleID != "general.gather_information" || action.SavingsAmount != nil {
			t.Fatalf("legacy response changed: %+v", action)
		}
	}
	action := request("?capability=gas_dryer_recommendations_v1")
	if action.RuleID != "dryer.gas_cycle_options" || action.State != "active" || len(action.EvidenceRefs) != 3 || action.SavingsAmount != nil {
		t.Fatalf("gas action: %+v", action)
	}
}
