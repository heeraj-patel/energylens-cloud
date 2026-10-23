package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/weather"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	wxHotDevice = "33333333-3333-4333-8333-333333333333"
	wxHotHome   = "44444444-4444-4444-8444-444444444444"
	wxLegacy    = "55555555-5555-4555-8555-555555555555"
)

func weatherTip(id, context, savingsType string, categories ...string) datawizfacts.Fact {
	cats, _ := json.Marshal(categories)
	return datawizfacts.Fact{
		ID: id, Route: "efficiency", SubjectKey: "efficiency:" + context, Field: "efficiency_tip", RecordKey: id,
		Value:  json.RawMessage(fmt.Sprintf(`{"context":%q,"applicable_categories":%s,"savings_type":%q,"action_source_text":"verbatim %s"}`, context, cats, savingsType, id)),
		Source: json.RawMessage(`{"url":"https://example.org/tip"}`), CollectedAt: "2026-10-21T00:00:00Z",
	}
}

func tipIDs(facts []datawizfacts.Fact) []string {
	out := []string{}
	for _, f := range facts {
		out = append(out, f.ID)
	}
	return out
}

func hvacWith(state *WeatherState, tips ...datawizfacts.Fact) Appliance {
	return Appliance{ID: "hvac-1", Category: "hvac", Fuel: "electricity", ResearchTips: tips, Weather: state}
}

func TestWeatherTipsNeedAnActiveTrigger(t *testing.T) {
	tips := []datawizfacts.Fact{weatherTip("hot", "weather_hot", "energy_reduction", "hvac"), weatherTip("cold", "weather_cold", "energy_reduction", "hvac")}
	for name, state := range map[string]*WeatherState{"legacy": nil, "no trigger": {}, "cold only for hot tip": {ColdRun: false}} {
		for _, action := range EvaluateGasAppliances(hvacWith(state, tips...)) {
			if len(action.ResearchEvidence) != 0 || action.WeatherContext != nil {
				t.Fatalf("%s: weather tip attached: %+v", name, action)
			}
		}
	}
}

func TestWeatherAlertAndRunEachFire(t *testing.T) {
	tips := []datawizfacts.Fact{weatherTip("hot", "weather_hot", "energy_reduction", "hvac"), weatherTip("cold", "weather_cold", "energy_reduction", "hvac")}
	for _, c := range []struct {
		state       WeatherState
		want, wantT string
	}{
		{WeatherState{HeatAlert: true}, "hot", "heat_alert"},
		{WeatherState{HotRun: true}, "hot", "hot_run"},
		{WeatherState{ColdAlert: true}, "cold", "cold_alert"},
		{WeatherState{ColdRun: true}, "cold", "cold_run"},
		{WeatherState{HotRun: true, HeatAlert: true}, "hot", "heat_alert"},
	} {
		state := c.state
		actions := EvaluateGasAppliances(hvacWith(&state, tips...))
		if len(actions) != 1 || len(actions[0].ResearchEvidence) != 1 || actions[0].ResearchEvidence[0].ID != c.want {
			t.Fatalf("%+v: %+v", c.state, actions)
		}
		if got := actions[0].WeatherContext; got == nil || got.Trigger != c.wantT || got.TOUSavingPerKWh != "" {
			t.Fatalf("%+v: context %+v", c.state, got)
		}
	}
}

func TestWeatherTipsGoFirstWithinTheCap(t *testing.T) {
	cat := func(id, ctx string) datawizfacts.Fact { return researchTip(id, ctx) }
	tips := []datawizfacts.Fact{
		cat("night", "night"), cat("maint", "maintenance"), cat("habit", "habits"),
		weatherTip("w-run", "weather_cold", "energy_reduction", "hvac"),
		weatherTip("w-alert", "weather_hot", "energy_reduction", "hvac"),
	}
	state := &WeatherState{HeatAlert: true, ColdRun: true}
	actions := EvaluateGasAppliances(hvacWith(state, tips...))
	if got := tipIDs(actions[0].ResearchEvidence); strings.Join(got, ",") != "w-alert,w-run,night" {
		t.Fatalf("attached %v", got)
	}
	state = &WeatherState{HotRun: true}
	actions = EvaluateGasAppliances(hvacWith(state, tips[:3:3]...))
	if strings.Join(tipIDs(actions[0].ResearchEvidence), ",") != "night,maint,habit" || actions[0].WeatherContext != nil {
		t.Fatalf("inactive context changed the legacy result: %v", tipIDs(actions[0].ResearchEvidence))
	}
}

func TestWeatherKeepsAtMostOneGeneralTip(t *testing.T) {
	tips := []datawizfacts.Fact{
		habitsTip("h1"), habitsTip("h2"), habitsTip("h3"),
		weatherTip("w1", "weather_hot", "energy_reduction", "hvac"),
	}
	actions := EvaluateGasAppliances(hvacWith(&WeatherState{HotRun: true}, tips...))
	if got := strings.Join(tipIDs(actions[0].ResearchEvidence), ","); got != "w1,h1" {
		t.Fatalf("attached %v", got)
	}
}

func habitsTip(id string) datawizfacts.Fact {
	tip := researchTip(id, "habits")
	tip.Value = json.RawMessage(`{"context":"habits","applicable_categories":[]}`)
	return tip
}

func TestHomeSlotHoldsThreeAlertBackedFirst(t *testing.T) {
	tips := []datawizfacts.Fact{
		weatherTip("cold-run", "weather_cold", "energy_reduction", "home"),
		weatherTip("hot-run-1", "weather_hot", "energy_reduction", "home"),
		weatherTip("hot-run-2", "weather_hot", "energy_reduction", "home"),
		weatherTip("device", "weather_hot", "energy_reduction", "hvac"),
		weatherTip("cold-alert", "weather_cold", "energy_reduction", "home"),
	}
	got := homeTips(tips, &WeatherState{ColdAlert: true, ColdRun: true, HotRun: true})
	var order []string
	for _, tip := range got {
		order = append(order, tip.Fact.ID+":"+tip.Trigger)
	}
	want := "cold-run:cold_alert,cold-alert:cold_alert,hot-run-1:hot_run"
	if strings.Join(order, ",") != want {
		t.Fatalf("home tips %v, want %s", order, want)
	}
	if len(homeTips(tips, &WeatherState{})) != 0 || len(homeTips(tips, nil)) != 0 {
		t.Fatal("inactive triggers produced home tips")
	}
}

func TestWeatherCostLineOnlyForShiftTipsOnATOUDay(t *testing.T) {
	tou := &householdenergy.TOUDay{Currency: "USD", SavingPerKWh: big.NewRat(25, 100), PeakStart: "16:00", PeakEnd: "21:00"}
	tips := []datawizfacts.Fact{weatherTip("shift", "weather_hot", "load_shift", "hvac"), weatherTip("reduce", "weather_hot", "energy_reduction", "hvac")}
	for name, c := range map[string]struct {
		tou  *householdenergy.TOUDay
		tips []datawizfacts.Fact
		cost bool
	}{
		"flat plan":        {nil, tips, false},
		"tou, shift tip":   {tou, tips[:1], true},
		"tou, reduce tip":  {tou, tips[1:], false},
		"tou, shift later": {tou, []datawizfacts.Fact{tips[1], tips[0]}, true},
	} {
		got := EvaluateGasAppliances(hvacWith(&WeatherState{HotRun: true, TOU: c.tou, MeasuredUse: true}, c.tips...))[0].WeatherContext
		if got == nil || !got.MeasuredUse {
			t.Fatalf("%s: %+v", name, got)
		}
		if has := got.TOUSavingPerKWh != ""; has != c.cost {
			t.Fatalf("%s: cost line %+v", name, got)
		}
		if c.cost && (got.TOUSavingPerKWh != "0.250000" || got.Currency != "USD" || got.PeakStartLocal != "16:00" || got.PeakEndLocal != "21:00") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

type fakeWeather struct {
	snap  weather.Snapshot
	err   error
	calls int
}

func (f *fakeWeather) Snapshot(context.Context, string, string, time.Time) (weather.Snapshot, error) {
	f.calls++
	return f.snap, f.err
}

type fakeTOU struct{ day *householdenergy.TOUDay }

func (f fakeTOU) TOUDay(context.Context, string, string, time.Time) (*householdenergy.TOUDay, error) {
	return f.day, nil
}

type fakeWaste struct{ ids []string }

func (f fakeWaste) DeviceMonthlyWaste(context.Context, string, string) ([]householdenergy.DeviceWaste, error) {
	out := []householdenergy.DeviceWaste{}
	for _, id := range f.ids {
		out = append(out, householdenergy.DeviceWaste{ApplianceID: id, MonthlyWaste: big.NewRat(1, 1)})
	}
	return out, nil
}

type weatherStubStore struct {
	*researchStubStore
	weatherTips []datawizfacts.Fact
}

func (s weatherStubStore) WeatherTips(context.Context) ([]datawizfacts.Fact, error) {
	return s.weatherTips, nil
}

func getRecommendations(t *testing.T, s Service, query string) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	s.Auth = func(next http.Handler) http.Handler { return next }
	s.UserID = func(*http.Request) string { return "owner" }
	s.Routes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+uuid.NewString()+"/recommendations"+query, nil))
	return w.Code, w.Body.Bytes()
}

func weatherService(f *fakeWeather) (Service, string, string) {
	metered, unmetered := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	hot := weatherTip(wxHotDevice, "weather_hot", "load_shift", "hvac")
	home := weatherTip(wxHotHome, "weather_hot", "energy_reduction", "home")
	habit := researchTip(wxLegacy, "habits")
	base := &researchStubStore{stubStore: &stubStore{appliances: []Appliance{
		{ID: metered, Category: "hvac", Fuel: "electricity", ResearchTips: []datawizfacts.Fact{habit}},
		{ID: unmetered, Category: "hvac", Fuel: "electricity"},
	}}, tips: []datawizfacts.Fact{habit}}
	tou := &householdenergy.TOUDay{Currency: "USD", SavingPerKWh: big.NewRat(1, 4), PeakStart: "16:00", PeakEnd: "21:00"}
	return Service{Store: weatherStubStore{base, []datawizfacts.Fact{hot, home}}, Weather: f, TOU: fakeTOU{tou}, Waste: fakeWaste{[]string{metered}}}, metered, unmetered
}

func availableSnapshot() weather.Snapshot {
	headline := "Heat Advisory issued"
	return weather.Snapshot{
		Status: weather.StatusAvailable, Provider: weather.ProviderNWS,
		Alerts: []weather.Alert{
			{ID: uuid.NewString(), Provider: weather.ProviderNWS, Event: "Heat Advisory", Headline: &headline, StartsAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), EndsAt: time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC), SourceURL: "https://api.weather.gov/alerts/x", Kind: weather.KindHot},
			{ID: uuid.NewString(), Provider: weather.ProviderNWS, Event: "Flood Watch", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), SourceURL: "https://api.weather.gov/alerts/y"},
		},
		Hot: weather.Run{Days: 12, LastDay: "2026-07-01", Active: true},
	}
}

type weatherBody struct {
	Actions []Action     `json:"actions"`
	Weather *WeatherPage `json:"weather"`
}

func TestWeatherCapabilityServesBlockAndDeviceContext(t *testing.T) {
	f := &fakeWeather{snap: availableSnapshot()}
	s, metered, unmetered := weatherService(f)
	code, raw := getRecommendations(t, s, "?capability=weather_tips_v1")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	var body weatherBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	w := body.Weather
	if w == nil || w.Status != "available" || w.Provider != "nws" || w.Attribution != "" || len(w.Alerts) != 1 || w.Alerts[0].Event != "Heat Advisory" ||
		len(w.Runs) != 1 || w.Runs[0].Kind != "hot" || w.Runs[0].Days != 12 || len(w.HomeTips) != 1 || w.HomeTips[0].Trigger != "heat_alert" || w.HomeTips[0].Fact.ID != wxHotHome {
		t.Fatalf("weather block %+v", w)
	}
	byID := map[string]Action{}
	for _, a := range body.Actions {
		byID[a.ApplianceID] = a
	}
	if c := byID[metered].WeatherContext; c == nil || !c.MeasuredUse || c.Trigger != "heat_alert" || c.TOUSavingPerKWh != "0.250000" || c.PeakStartLocal != "16:00" {
		t.Fatalf("metered context %+v", c)
	}
	if c := byID[unmetered].WeatherContext; c == nil || c.MeasuredUse {
		t.Fatalf("unmetered context %+v", c)
	}
	if got := strings.Join(tipIDs(byID[metered].ResearchEvidence), ","); got != wxHotDevice+","+wxLegacy {
		t.Fatalf("metered tips %v", got)
	}
}

func TestWeatherFetchFailureStillServesTheRestOfThePage(t *testing.T) {
	for name, f := range map[string]*fakeWeather{
		"error":       {err: errors.New("db down")},
		"unavailable": {snap: weather.Snapshot{Status: weather.StatusUnavailable, Reason: weather.ReasonProviderUnavailable}},
		"unsupported": {snap: weather.Snapshot{Status: weather.StatusUnavailable, Reason: weather.ReasonUnsupportedLocation}},
	} {
		s, _, _ := weatherService(f)
		code, raw := getRecommendations(t, s, "?capability=weather_tips_v1")
		var body weatherBody
		if code != http.StatusOK || json.Unmarshal(raw, &body) != nil || body.Weather == nil || body.Weather.Status != "unavailable" || body.Weather.Reason == "" {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
		if len(body.Actions) != 2 || len(body.Weather.HomeTips) != 0 || len(body.Weather.Alerts) != 0 {
			t.Fatalf("%s: %s", name, raw)
		}
		for _, a := range body.Actions {
			if a.WeatherContext != nil || strings.Contains(strings.Join(tipIDs(a.ResearchEvidence), ","), "33333333-") {
				t.Fatalf("%s: weather tip attached: %+v", name, a)
			}
		}
	}
	s, _, _ := weatherService(nil)
	s.Weather = nil
	code, raw := getRecommendations(t, s, "?capability=weather_tips_v1")
	if code != http.StatusOK || !strings.Contains(string(raw), `"status":"unavailable"`) {
		t.Fatalf("no weather service: %d %s", code, raw)
	}
}

func TestLegacyCapabilitiesAreByteIdenticalAndNeverFetchWeather(t *testing.T) {
	f := &fakeWeather{snap: availableSnapshot()}
	withWeather, _, _ := weatherService(f)
	plain, _, _ := weatherService(nil)
	plain.Weather, plain.TOU = nil, nil
	plain.Store = plain.Store.(weatherStubStore).researchStubStore
	withWeather.Store = withWeather.Store.(weatherStubStore)
	for _, query := range []string{"", "?capability=gas_dryer_recommendations_v1", "?capability=gas_appliance_recommendations_v1", "?capability=unknown"} {
		code, a := getRecommendations(t, withWeather, query)
		_, b := getRecommendations(t, plain, query)
		strip := regexp.MustCompile(`"as_of":"[^"]*"`)
		if code != http.StatusOK || strip.ReplaceAllString(string(a), "") != strip.ReplaceAllString(string(b), "") {
			t.Fatalf("%q differs:\n%s\n%s", query, a, b)
		}
		if strings.Contains(string(a), "weather") {
			t.Fatalf("%q mentions weather: %s", query, a)
		}
	}
	if f.calls != 0 {
		t.Fatalf("legacy capability fetched weather %d times", f.calls)
	}
}

var (
	decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	clockPattern   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func keysOf(t *testing.T, raw json.RawMessage, allowed ...string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			t.Fatalf("key %q is not in the contract: %s", k, raw)
		}
	}
	return m
}

// The page's keys and formats follow api/cloud.schema.json RecommendationPage;
// the contracts repo validates the same shapes with the schema itself.
func TestWeatherPageMatchesContractShape(t *testing.T) {
	f := &fakeWeather{snap: availableSnapshot()}
	f.snap.Provider = weather.ProviderECCC
	s, _, _ := weatherService(f)
	_, raw := getRecommendations(t, s, "?capability=weather_tips_v1")
	t.Logf("page: %s", raw)
	page := keysOf(t, raw, "actions", "as_of", "research_facts", "weather")
	block := keysOf(t, page["weather"], "status", "reason", "provider", "attribution", "alerts", "runs", "home_tips")
	for _, k := range []string{"status", "provider", "attribution", "alerts", "runs", "home_tips"} {
		if _, ok := block[k]; !ok {
			t.Fatalf("available block lacks %s", k)
		}
	}
	if string(block["attribution"]) != `"Data Source: Environment and Climate Change Canada"` {
		t.Fatalf("attribution %s", block["attribution"])
	}
	var alerts, runs, tips []json.RawMessage
	_ = json.Unmarshal(block["alerts"], &alerts)
	_ = json.Unmarshal(block["runs"], &runs)
	_ = json.Unmarshal(block["home_tips"], &tips)
	alert := keysOf(t, alerts[0], "event", "headline", "starts_at", "ends_at", "source_url", "evidence_ref")
	keysOf(t, alert["evidence_ref"], "id", "kind", "revision")
	keysOf(t, runs[0], "kind", "days", "last_day")
	keysOf(t, tips[0], "fact", "trigger")
	var actions []struct {
		Weather json.RawMessage `json:"weather_context"`
	}
	_ = json.Unmarshal(page["actions"], &actions)
	seen := false
	for _, a := range actions {
		if a.Weather == nil {
			continue
		}
		seen = true
		ctx := keysOf(t, a.Weather, "trigger", "measured_use", "tou_saving_per_kwh", "currency", "peak_start_local", "peak_end_local")
		var saving, start, end string
		_ = json.Unmarshal(ctx["tou_saving_per_kwh"], &saving)
		_ = json.Unmarshal(ctx["peak_start_local"], &start)
		_ = json.Unmarshal(ctx["peak_end_local"], &end)
		if !decimalPattern.MatchString(saving) || !clockPattern.MatchString(start) || !clockPattern.MatchString(end) || string(ctx["currency"]) != `"USD"` {
			t.Fatalf("context formats %s", a.Weather)
		}
	}
	if !seen {
		t.Fatal("no action carried weather_context")
	}
}
