package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizfacts"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type stubStore struct {
	appliances  []Appliance
	err         error
	owner, home string
}

type researchStubStore struct {
	*stubStore
	tips []datawizfacts.Fact
}

func (s *researchStubStore) Research(context.Context) ([]datawizfacts.Fact, error) {
	return s.tips, nil
}

func (s *stubStore) Current(_ context.Context, owner, home string, _ time.Time) ([]Appliance, error) {
	s.owner, s.home = owner, home
	return s.appliances, s.err
}

func TestRecommendationsRouteCapsEachApplianceAndKeepsResearchFacts(t *testing.T) {
	contexts := []string{"night", "away", "peak_usage", "habits", "appliance_type", "upgrades", "maintenance"}
	tips := make([]datawizfacts.Fact, len(contexts))
	for i, context := range contexts {
		tips[i] = researchTip(uuid.NewString(), context)
	}
	store := &researchStubStore{stubStore: &stubStore{appliances: []Appliance{
		{ID: uuid.NewString(), Category: "hvac", Fuel: "electricity", ResearchTips: tips},
		{ID: uuid.NewString(), Category: "hvac", Fuel: "electricity", ResearchTips: tips},
	}}, tips: tips}
	r := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+uuid.NewString()+"/recommendations", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("response: %d", w.Code)
	}
	var body struct {
		Actions  []Action            `json:"actions"`
		Research []datawizfacts.Fact `json:"research_facts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Actions) != 2 || len(body.Research) != len(tips) {
		t.Fatalf("actions/research counts: %d/%d", len(body.Actions), len(body.Research))
	}
	wantDevices := map[string]bool{}
	for _, appliance := range store.appliances {
		wantDevices[appliance.ID] = true
	}
	for _, action := range body.Actions {
		if !wantDevices[action.ApplianceID] {
			t.Fatalf("unexpected or repeated device: %s", action.ApplianceID)
		}
		delete(wantDevices, action.ApplianceID)
		assertTipAttachments(t, action, tips[:3])
	}
	if len(wantDevices) != 0 {
		t.Fatalf("missing devices: %+v", wantDevices)
	}
	for i, fact := range tips {
		if body.Research[i].ID != fact.ID || string(body.Research[i].Value) != string(fact.Value) || string(body.Research[i].Source) != string(fact.Source) {
			t.Fatalf("research fact %d changed: %+v", i, body.Research[i])
		}
	}
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

type recordStore struct {
	*stubStore
	recorded []LifecycleEvent
}

func (s *recordStore) Latest(context.Context, string, string) (map[string]LifecycleEvent, error) {
	return map[string]LifecycleEvent{}, nil
}
func (s *recordStore) Replay(context.Context, string, string, string) (*LifecycleEvent, error) {
	return nil, nil
}
func (s *recordStore) Record(_ context.Context, _, _, _ string, e LifecycleEvent) (LifecycleEvent, error) {
	s.recorded = append(s.recorded, e)
	return e, nil
}

func TestRecordUsesCapabilityRuleVocabulary(t *testing.T) {
	id := uuid.NewString()
	store := &recordStore{stubStore: &stubStore{appliances: []Appliance{{ID: id, Category: "furnace", Fuel: "natural_gas", Identity: "unresolved"}}}}
	r := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	post := func(query string) int {
		body := `{"appliance_id":"` + id + `","rule_id":"furnace.gas_efficiency_check","rule_version":1,"state":"dismissed"}`
		req := httptest.NewRequest(http.MethodPost, "/homes/"+uuid.NewString()+"/recommendation-events"+query, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", uuid.NewString())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("?capability=gas_appliance_recommendations_v1"); code != 200 || len(store.recorded) != 1 {
		t.Fatalf("gas appliance capability: %d recorded %d", code, len(store.recorded))
	}
	if code := post(""); code != http.StatusConflict {
		t.Fatalf("legacy capability: %d", code)
	}
}
