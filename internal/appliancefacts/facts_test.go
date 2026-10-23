package appliancefacts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizresolution"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/questions"
	"energylens.local/cloud/internal/recommendation"
	"github.com/go-chi/chi/v5"
)

const (
	homeID  = "00000000-0000-4000-8000-000000000001"
	applID  = "00000000-0000-4000-8000-000000000003"
	otherID = "00000000-0000-4000-8000-000000000099"
)

type stub struct {
	appliance  inventory.Appliance
	resolution json.RawMessage
	resErr     error
	runs       []json.RawMessage
	runsErr    error
	programs   []programs.OwnerProgram
	progErr    error
	current    []recommendation.Appliance
	answers    []questions.Answer
	eff        *householdenergy.MeasuredEfficiency
	effErr     error
	fp         string
	fpCalls    int
	listCalls  int
}

func (s *stub) Get(_ context.Context, owner, home, id string) (inventory.Appliance, error) {
	if owner != "owner" || home != homeID || id != s.appliance.ID {
		return inventory.Appliance{}, inventory.ErrNotFound
	}
	return s.appliance, nil
}
func (s *stub) ListWithRevision(context.Context, string, string) ([]inventory.Appliance, string, error) {
	return nil, "", inventory.ErrNotFound
}
func (s *stub) ForOwner(context.Context, string, string, string) (json.RawMessage, error) {
	return s.resolution, s.resErr
}
func (s *stub) ListRuns(context.Context, string, string, *string, time.Time) ([]json.RawMessage, error) {
	s.listCalls++
	return s.runs, s.runsErr
}
func (s *stub) List(context.Context, string, string, string, time.Time) (programs.OwnerProgramResult, error) {
	return programs.OwnerProgramResult{Programs: s.programs}, s.progErr
}
func (s *stub) ListHome(context.Context, string, string, time.Time) (programs.OwnerProgramResult, error) {
	return programs.OwnerProgramResult{}, nil
}
func (s *stub) Current(context.Context, string, string, time.Time) ([]recommendation.Appliance, error) {
	return s.current, nil
}
func (s *stub) CurrentAnswers(context.Context, string, string, string) ([]questions.Answer, error) {
	return s.answers, nil
}
func (s *stub) TrailingEfficiency(context.Context, string, string) (*householdenergy.MeasuredEfficiency, error) {
	return s.eff, s.effErr
}
func (s *stub) Fingerprint(context.Context, string, string, time.Time) (string, error) {
	s.fpCalls++
	return s.fp, nil
}

func handler(s *stub) http.Handler {
	svc := Service{
		Composer: Composer{Appliances: s, Resolutions: s, Estimates: s, Programs: s, Recommendations: s, Answers: s, Efficiency: s, Revisions: s},
		Auth:     func(next http.Handler) http.Handler { return next },
		UserID:   func(*http.Request) string { return "owner" },
	}
	r := chi.NewRouter()
	svc.Routes(r)
	return r
}

func electric() *stub {
	return &stub{
		appliance:  inventory.Appliance{ID: applID, HomeID: homeID, Name: "Washer", Category: "washer", Identity: inventory.Identity{Fuel: "electricity", MatchQuality: "unresolved"}, Version: 2},
		resolution: json.RawMessage(`{"status":"resolved"}`),
		runs:       []json.RawMessage{json.RawMessage(`{"kind":"estimated_operating_cost","id":"e"}`)},
		programs:   []programs.OwnerProgram{{ProgramID: "p1"}, {ProgramID: "p2"}},
		current:    []recommendation.Appliance{{ID: applID, Category: "washer", Fuel: "electricity", Identity: "unresolved"}},
		answers:    []questions.Answer{{AnswerCreate: questions.AnswerCreate{QuestionID: "washer.cycles_per_week"}}},
		eff: &householdenergy.MeasuredEfficiency{
			AwaySource:  "default_schedule",
			NightWindow: householdenergy.EfficiencyNightWindow{StartLocal: "22:00", EndLocal: "06:00", Timezone: "UTC"},
			Devices: []householdenergy.DeviceEfficiency{
				{ApplianceID: otherID},
				{ApplianceID: applID, Basis: householdenergy.DeviceEfficiencyBasis{Night: "measured", Away: "measured", Peak: "measured", Habits: "estimated"}},
			},
		},
		fp: "v1",
	}
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]map[string]any {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out map[string]map[string]any
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	out = map[string]map[string]any{}
	for k, v := range raw {
		var m map[string]any
		if json.Unmarshal(v, &m) == nil {
			out[k] = m
		}
	}
	return out
}

func TestFactsAllSections(t *testing.T) {
	rec := get(handler(electric()), "/homes/"+homeID+"/appliances/"+applID+"/facts")
	body := decode(t, rec)
	for _, name := range []string{"identity", "catalog_facts", "estimate", "programs", "tips", "usage_answers"} {
		if body[name]["status"] != "ok" {
			t.Fatalf("%s: %v", name, body[name])
		}
	}
	eff := body["efficiency"]
	dev, _ := eff["device"].(map[string]any)
	if eff["status"] != "ok" || eff["window"] != "trailing_30_days" || eff["away_source"] != "default_schedule" || eff["night_window"] == nil || dev["appliance_id"] != applID {
		t.Fatalf("efficiency: %v", eff)
	}
	if body["programs"]["total"] != float64(2) {
		t.Fatalf("programs: %v", body["programs"])
	}
	if rec.Header().Get("Cache-Control") != "private, no-cache" || rec.Header().Get("ETag") == "" {
		t.Fatalf("headers %v", rec.Header())
	}
}

func TestOneSectionFailsOthersStayOK(t *testing.T) {
	s := electric()
	s.runsErr = errors.New("boom")
	body := decode(t, get(handler(s), "/homes/"+homeID+"/appliances/"+applID+"/facts"))
	if body["estimate"]["status"] != "error" || body["estimate"]["estimate"] != nil {
		t.Fatalf("estimate: %v", body["estimate"])
	}
	for _, name := range []string{"identity", "catalog_facts", "programs", "usage_answers"} {
		if body[name]["status"] != "ok" {
			t.Fatalf("%s: %v", name, body[name])
		}
	}
}

func TestGasDevice(t *testing.T) {
	s := electric()
	s.appliance.Category = "furnace"
	s.appliance.Identity.Fuel = "natural_gas"
	s.resErr = datawizresolution.ErrNotFound
	s.resolution = nil
	s.runs = []json.RawMessage{json.RawMessage(`{"kind":"unavailable","unavailable_reason":"basis_incomparable"}`)}
	s.programs = nil
	s.progErr = programs.ErrLocationRequired
	s.answers = nil
	s.current = []recommendation.Appliance{{ID: applID, Category: "furnace", Fuel: "natural_gas", Identity: "unresolved"}}
	body := decode(t, get(handler(s), "/homes/"+homeID+"/appliances/"+applID+"/facts?capability=gas_appliance_recommendations_v1"))
	if body["catalog_facts"]["status"] != "empty" {
		t.Fatalf("catalog: %v", body["catalog_facts"])
	}
	if body["estimate"]["status"] != "unavailable" || body["estimate"]["reason"] != "basis_incomparable" {
		t.Fatalf("estimate: %v", body["estimate"])
	}
	if body["programs"]["status"] != "empty" || body["programs"]["reason"] != "location_required" || body["programs"]["total"] != float64(0) {
		t.Fatalf("programs: %v", body["programs"])
	}
	if body["usage_answers"]["status"] != "empty" || body["identity"]["status"] != "ok" {
		t.Fatalf("answers/identity: %v %v", body["usage_answers"], body["identity"])
	}
}

func TestGasFurnaceTipsFollowRecommendationCapability(t *testing.T) {
	s := electric()
	s.appliance.Category = "furnace"
	s.appliance.Identity.Fuel = "natural_gas"
	s.current = []recommendation.Appliance{{ID: applID, Category: "furnace", Fuel: "natural_gas", Identity: "unresolved"}}
	has := func(query string) bool {
		rec := get(handler(s), "/homes/"+homeID+"/appliances/"+applID+"/facts"+query)
		var body struct {
			Tips struct {
				Items []struct {
					RuleID string `json:"rule_id"`
				} `json:"items"`
			} `json:"tips"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		for _, it := range body.Tips.Items {
			if it.RuleID == "furnace.gas_efficiency_check" {
				return true
			}
		}
		return false
	}
	if !has("?capability=gas_appliance_recommendations_v1") {
		t.Fatal("gas appliance capability must include furnace.gas_efficiency_check")
	}
	if has("?capability=gas_dryer_recommendations_v1") || has("") {
		t.Fatal("dryer-only and legacy capabilities must not include furnace rules")
	}
}

func TestIfNoneMatchReturns304WithoutComposing(t *testing.T) {
	s := electric()
	h := handler(s)
	first := get(h, "/homes/"+homeID+"/appliances/"+applID+"/facts")
	tag := first.Header().Get("ETag")
	s.listCalls = 0
	rec := get(h, "/homes/"+homeID+"/appliances/"+applID+"/facts", "If-None-Match", tag)
	if rec.Code != 304 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != tag || s.listCalls != 0 {
		t.Fatalf("status %d body %q calls %d", rec.Code, rec.Body, s.listCalls)
	}
	s.fp = "v2"
	if rec = get(h, "/homes/"+homeID+"/appliances/"+applID+"/facts", "If-None-Match", tag); rec.Code != 200 {
		t.Fatalf("changed revision returned %d", rec.Code)
	}
	cap := get(h, "/homes/"+homeID+"/appliances/"+applID+"/facts?capability=gas_dryer_recommendations_v1")
	if cap.Header().Get("ETag") == tag {
		t.Fatal("capability must change the ETag")
	}
}

func TestOtherHomesApplianceIs404(t *testing.T) {
	s := electric()
	h := handler(s)
	for _, path := range []string{
		"/homes/" + otherID + "/appliances/" + applID + "/facts",
		"/homes/" + homeID + "/appliances/" + otherID + "/facts",
		"/homes/not-a-uuid/appliances/" + applID + "/facts",
	} {
		if rec := get(h, path); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if rec := get(h, "/homes/"+homeID+"/appliances/"+applID+"/facts?capability=bogus"); rec.Code != 400 {
		t.Fatalf("bogus capability: %d", rec.Code)
	}
}

func TestEfficiencySectionStates(t *testing.T) {
	path := "/homes/" + homeID + "/appliances/" + applID + "/facts"
	s := electric()
	s.effErr = errors.New("boom")
	body := decode(t, get(handler(s), path))
	if body["efficiency"]["status"] != "error" || body["efficiency"]["device"] != nil {
		t.Fatalf("efficiency: %v", body["efficiency"])
	}
	for _, name := range []string{"identity", "catalog_facts", "estimate", "programs", "usage_answers"} {
		if body[name]["status"] != "ok" {
			t.Fatalf("%s: %v", name, body[name])
		}
	}
	s = electric()
	s.eff = nil
	if e := decode(t, get(handler(s), path))["efficiency"]; e["status"] != "unavailable" || e["device"] != nil {
		t.Fatalf("no measured efficiency: %v", e)
	}
	s = electric()
	s.eff.Devices = s.eff.Devices[:1]
	if e := decode(t, get(handler(s), path))["efficiency"]; e["status"] != "unavailable" || e["reason"] != "appliance_not_in_efficiency" {
		t.Fatalf("missing device: %v", e)
	}
}

func TestGasDeviceEfficiencyBasis(t *testing.T) {
	s := electric()
	s.appliance.Identity.Fuel = "natural_gas"
	s.eff.Devices[1].Basis = householdenergy.DeviceEfficiencyBasis{Night: "fuel_not_applicable", Away: "fuel_not_applicable", Peak: "fuel_not_applicable", Habits: "fuel_not_applicable"}
	e := decode(t, get(handler(s), "/homes/"+homeID+"/appliances/"+applID+"/facts"))["efficiency"]
	basis := e["device"].(map[string]any)["basis"].(map[string]any)
	if e["status"] != "ok" || basis["night"] != "fuel_not_applicable" || basis["habits"] != "fuel_not_applicable" {
		t.Fatalf("gas efficiency: %v", e)
	}
}
