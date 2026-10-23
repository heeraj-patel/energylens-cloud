package recommendation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizfacts"
)

const peakSource = "https://austinenergy.com/energy-efficiency/rebates-incentives/residential/ps-volunteer"

func peakTip(id, sentence string, providers []string, categories ...string) datawizfacts.Fact {
	cats, _ := json.Marshal(categories)
	ids, _ := json.Marshal(providers)
	return datawizfacts.Fact{
		ID: id, Route: "efficiency", SubjectKey: "efficiency:peak_demand_day", Field: "efficiency_tip", RecordKey: "official:peak_demand_day:" + id,
		Value:  json.RawMessage(fmt.Sprintf(`{"context":"peak_demand_day","applicable_categories":%s,"applicable_provider_ids":%s,"action_source_text":%q}`, cats, ids, sentence)),
		Source: json.RawMessage(`{"url":"` + peakSource + `"}`), CollectedAt: "2026-10-20T00:00:00Z",
	}
}

type peakStubStore struct {
	weatherStubStore
	tips  []datawizfacts.Fact
	home  PeakDemandHome
	reads int
}

func (s *peakStubStore) PeakDemandTips(context.Context) ([]datawizfacts.Fact, error) {
	s.reads++
	return s.tips, nil
}

func (s *peakStubStore) PeakDemandHome(context.Context, string, string, time.Time) (PeakDemandHome, error) {
	return s.home, nil
}

func austin(month time.Month, day int) PeakDemandHome {
	return PeakDemandHome{EIAID: "1015", Local: time.Date(2026, month, day, 12, 0, 0, 0, time.UTC)}
}

func peakService(home PeakDemandHome, tips ...datawizfacts.Fact) (Service, *peakStubStore) {
	s, _, _ := weatherService(&fakeWeather{snap: availableSnapshot()})
	store := &peakStubStore{weatherStubStore: s.Store.(weatherStubStore), tips: tips, home: home}
	s.Store = store
	return s, store
}

var austinTips = []datawizfacts.Fact{
	peakTip("pd-dryer", "Delay the dryer or dishwasher", []string{"1015"}, "dryer", "dishwasher"),
	peakTip("pd-hvac", "Adjust the thermostat a few degrees", []string{"1015"}, "hvac", "air_conditioner", "thermostat"),
	peakTip("pd-fans", "Use fans", []string{"1015"}, "home"),
	peakTip("pd-fans-dup", "Use fans", []string{"1015"}, "home"),
	peakTip("pd-blinds", "Keep blinds closed", []string{"1015"}, "home"),
	peakTip("pd-extra", "One more", []string{"1015"}, "home"),
	peakTip("pd-other-utility", "Other utility", []string{"9999"}, "home", "hvac"),
}

type peakPageBody struct {
	Actions    []Action        `json:"actions"`
	PeakDemand *PeakDemandPage `json:"peak_demand"`
}

func peakBody(t *testing.T, s Service, query string) (peakPageBody, []byte) {
	t.Helper()
	code, raw := getRecommendations(t, s, query)
	var body peakPageBody
	if code != http.StatusOK || json.Unmarshal(raw, &body) != nil {
		t.Fatalf("%d %s", code, raw)
	}
	return body, raw
}

func TestPeakDemandBlockEligibility(t *testing.T) {
	for name, c := range map[string]struct {
		home PeakDemandHome
		want bool
	}{
		"june 1":        {austin(time.June, 1), true},
		"september 30":  {austin(time.September, 30), true},
		"may 31":        {austin(time.May, 31), false},
		"october 1":     {austin(time.October, 1), false},
		"winter":        {austin(time.January, 15), false},
		"other utility": {PeakDemandHome{EIAID: "14328", Local: austin(time.July, 1).Local}, false},
		"no utility":    {PeakDemandHome{Local: austin(time.July, 1).Local}, false},
	} {
		s, _ := peakService(c.home, austinTips...)
		body, raw := peakBody(t, s, "?capability=peak_demand_v1")
		if (body.PeakDemand != nil) != c.want || !c.want && strings.Contains(string(raw), "peak_demand") {
			t.Fatalf("%s: block %+v", name, body.PeakDemand)
		}
		if !c.want {
			for _, a := range body.Actions {
				if a.PeakDemandContext != "" {
					t.Fatalf("%s: peak context on %+v", name, a)
				}
			}
		}
	}
}

func TestPeakDemandUsesTheHomesLocalDate(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	late := time.Date(2026, time.September, 30, 23, 30, 0, 0, loc).UTC()
	if !inPeakSeason(late.In(loc)) || inPeakSeason(late) {
		t.Fatal("local date must decide the season")
	}
}

func TestPeakDemandHomeSlotDedupesBySentenceAndCapsAtThree(t *testing.T) {
	s, _ := peakService(austin(time.July, 1), austinTips...)
	body, _ := peakBody(t, s, "?capability=peak_demand_v1")
	p := body.PeakDemand
	if p == nil || p.Status != "available" || p.WindowLocal != "13:00-19:00" || p.SourceURL != peakSource {
		t.Fatalf("block %+v", p)
	}
	var ids []string
	for _, tip := range p.HomeTips {
		ids = append(ids, tip.Fact.ID)
	}
	if strings.Join(ids, ",") != "pd-fans,pd-blinds,pd-extra" {
		t.Fatalf("home tips %v", ids)
	}
	if page, _ := peakDemandView(austinTips[:1], "1015"); page == nil || len(page.HomeTips) != 0 {
		t.Fatalf("device-only tips: %+v", page)
	}
}

func TestPeakDemandDeviceTipsFollowWeatherTipsWithinTheCap(t *testing.T) {
	s, _ := peakService(austin(time.July, 1), austinTips...)
	appliances, _, err := s.withWeather(context.Background(), "owner", "home", time.Now(), mustCurrent(t, s))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := s.withPeakDemand(context.Background(), "owner", "home", time.Now(), appliances)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out {
		ids := tipIDs(a.ResearchTips)
		if len(ids) < 2 || ids[0] != wxHotDevice || ids[1] != "pd-dryer" {
			t.Fatalf("order %v", ids)
		}
	}
	body, _ := peakBody(t, s, "?capability=peak_demand_v1")
	if len(body.Actions) != 2 {
		t.Fatalf("%d actions", len(body.Actions))
	}
	for _, a := range body.Actions {
		if a.PeakDemandContext != "peak_demand_day" || a.WeatherContext == nil || len(a.ResearchEvidence) > maxResearchTipsPerAppliance {
			t.Fatalf("action %+v", a)
		}
		ids := strings.Join(tipIDs(a.ResearchEvidence), ",")
		if !strings.Contains(ids, "pd-hvac") || strings.Contains(ids, "pd-other-utility") || strings.Contains(ids, "pd-fans") {
			t.Fatalf("tips %s", ids)
		}
	}
}

func TestPeakDemandTipsNeverExceedTheDeviceCap(t *testing.T) {
	tips := []datawizfacts.Fact{weatherTip("hot", "weather_hot", "energy_reduction", "hvac")}
	for i := 0; i < 4; i++ {
		tips = append(tips, peakTip(fmt.Sprintf("pd-%d", i), fmt.Sprintf("tip %d", i), []string{"1015"}, "hvac"))
	}
	actions := EvaluateGasAppliances(hvacWith(&WeatherState{HeatAlert: true}, tips...))
	if len(actions) != 1 || strings.Join(tipIDs(actions[0].ResearchEvidence), ",") != "hot,pd-0,pd-1" || actions[0].PeakDemandContext != "peak_demand_day" {
		t.Fatalf("%+v", actions)
	}
}

func mustCurrent(t *testing.T, s Service) []Appliance {
	t.Helper()
	a, err := s.Store.Current(context.Background(), "owner", "home", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLegacyAndWeatherCapabilitiesIgnorePeakDemand(t *testing.T) {
	f := &fakeWeather{snap: availableSnapshot()}
	plain, _, _ := weatherService(f)
	withPeak, store := peakService(austin(time.July, 1), austinTips...)
	withPeak.Weather = f
	strip := regexp.MustCompile(`"as_of":"[^"]*"`)
	for _, query := range []string{"", "?capability=gas_dryer_recommendations_v1", "?capability=gas_appliance_recommendations_v1", "?capability=weather_tips_v1", "?capability=unknown"} {
		_, a := getRecommendations(t, plain, query)
		_, b := getRecommendations(t, withPeak, query)
		if strip.ReplaceAllString(string(a), "") != strip.ReplaceAllString(string(b), "") {
			t.Fatalf("%q differs:\n%s\n%s", query, a, b)
		}
		if strings.Contains(string(b), "peak_demand") {
			t.Fatalf("%q mentions peak demand: %s", query, b)
		}
	}
	if store.reads != 0 {
		t.Fatalf("peak tips read %d times for other capabilities", store.reads)
	}
}

func TestPeakDemandPageMatchesContractShape(t *testing.T) {
	s, _ := peakService(austin(time.July, 1), austinTips...)
	_, raw := peakBody(t, s, "?capability=peak_demand_v1")
	page := keysOf(t, raw, "actions", "as_of", "research_facts", "weather", "peak_demand")
	block := keysOf(t, page["peak_demand"], "status", "window_local", "source_url", "home_tips")
	var tips []json.RawMessage
	if err := json.Unmarshal(block["home_tips"], &tips); err != nil || len(tips) == 0 {
		t.Fatalf("home_tips %s", block["home_tips"])
	}
	for _, tip := range tips {
		keysOf(t, tip, "fact")
	}
	if string(block["status"]) != `"available"` || !strings.HasPrefix(string(block["source_url"]), `"https://`) {
		t.Fatalf("block %s", page["peak_demand"])
	}
}
