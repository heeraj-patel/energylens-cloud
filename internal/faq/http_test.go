package faq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/internal/weather"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type stubStore struct {
	current          *CurrentEstimate
	weather          []weather.Observation
	locationRevision int64
	err              error
	called           bool
}

func (s *stubStore) CurrentEstimate(_ context.Context, _, _ string, _ *string, _ time.Time) (*CurrentEstimate, error) {
	s.called = true
	return s.current, s.err
}
func (s *stubStore) CurrentWeather(_ context.Context, _, _ string, _ time.Time) ([]weather.Observation, int64, error) {
	return s.weather, s.locationRevision, s.err
}

func faqRequest(t *testing.T, store *stubStore, path string) (int, Answer) {
	t.Helper()
	r := chi.NewRouter()
	Service{Store: store, Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return uuid.NewString() }}.Routes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var answer Answer
	if w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &answer) != nil {
		t.Fatal("invalid answer JSON")
	}
	return w.Code, answer
}

func TestFAQFallbackChecksOwnerAndPreservesExactAnswer(t *testing.T) {
	home := uuid.NewString()
	store := &stubStore{}
	status, answer := faqRequest(t, store, "/homes/"+home+"/faq/answers/high_bill_possible_contributors")
	if status != 200 || !store.called || answer.Text != Insufficient || answer.Status != "insufficient_information" || answer.NextAction == nil || answer.NextAction.Kind != "adaptive_questions" {
		t.Fatalf("fallback: %d %+v", status, answer)
	}
	store = &stubStore{err: ErrNotFound}
	status, _ = faqRequest(t, store, "/homes/"+home+"/faq/answers/high_bill_possible_contributors")
	if status != 404 || !store.called {
		t.Fatalf("cross-owner fallback: %d", status)
	}
}

func TestFAQCostUsesOnlyValidatedEstimate(t *testing.T) {
	home, appliance := uuid.NewString(), uuid.NewString()
	path := "/homes/" + home + "/faq/answers/device_operating_cost?appliance_id=" + appliance
	valid := `{"appliance_id":"` + appliance + `","kind":"estimated_operating_cost","period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"0.00","currency":"USD"},"evidence_refs":[{"id":"` + uuid.NewString() + `","kind":"catalog_fact","revision":1},{"id":"` + uuid.NewString() + `","kind":"tariff","revision":1}]}`
	store := &stubStore{current: &CurrentEstimate{Result: json.RawMessage(valid), AsOf: time.Now()}}
	status, answer := faqRequest(t, store, path)
	if status != 200 || answer.Status != "supported" || !strings.Contains(answer.Findings[0].Text, "0.00 USD") || len(answer.EvidenceRefs) != 2 {
		t.Fatalf("supported cost: %d %+v", status, answer)
	}
	store.current.Result = json.RawMessage(strings.Replace(valid, "estimated_operating_cost", "estimated_useful_cost", 1))
	status, answer = faqRequest(t, store, path)
	if status != 200 || answer.Text != Insufficient || len(answer.Findings) != 0 {
		t.Fatalf("unsupported cost: %d %+v", status, answer)
	}
}

func TestFAQHighBillUsesOnlyCompleteFreshWeatherContext(t *testing.T) {
	home := uuid.NewString()
	now := time.Now().UTC()
	// Use a real timezone accepted by the weather assembler.
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	local := now.In(location)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	var observations []weather.Observation
	for ago := 14; ago >= 1; ago-- {
		mean := 25.0
		if ago <= 7 {
			mean = 18
		}
		observations = append(observations, weather.Observation{ID: uuid.NewString(), Provider: "test", CellKey: "US:787:America/Chicago", Timezone: "America/Chicago", LocalDate: today.AddDate(0, 0, -ago).Format("2006-01-02"), MeanTemperatureC: mean, SourceRevision: "source-1", RetrievedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
	}
	store := &stubStore{weather: observations, locationRevision: 1}
	path := "/homes/" + home + "/faq/answers/high_bill_possible_contributors"
	status, answer := faqRequest(t, store, path)
	if status != 200 || answer.Status != "partial" || len(answer.Findings) != 1 || len(answer.EvidenceRefs) != 15 || !strings.Contains(answer.Findings[0].Text, "Coverage: 7 of 7") || strings.Contains(answer.Text, "bill comparison") {
		t.Fatalf("weather answer: %d %+v", status, answer)
	}
	store.weather[0].ExpiresAt = now.Add(-time.Minute)
	status, answer = faqRequest(t, store, path)
	if status != 200 || answer.Text != Insufficient || len(answer.Findings) != 0 || answer.NextAction == nil {
		t.Fatalf("expired fallback: %d %+v", status, answer)
	}
}

type wxRecStore struct{ tips []datawizfacts.Fact }

func (wxRecStore) Current(context.Context, string, string, time.Time) ([]recommendation.Appliance, error) {
	return nil, nil
}
func (s wxRecStore) WeatherTips(context.Context) ([]datawizfacts.Fact, error) { return s.tips, nil }

type wxSnapshot struct {
	snap weather.Snapshot
	err  error
}

func (f wxSnapshot) Snapshot(context.Context, string, string, time.Time) (weather.Snapshot, error) {
	return f.snap, f.err
}

func wxTip(id, context, url string) datawizfacts.Fact {
	return datawizfacts.Fact{ID: id, Field: "efficiency_tip", Value: json.RawMessage(`{"context":"` + context + `","applicable_categories":["home"],"action_source_text":"Tip ` + id + `"}`),
		Source: json.RawMessage(`{"url":"` + url + `"}`)}
}

func wxRouter(snap wxSnapshot, tips []datawizfacts.Fact) *chi.Mux {
	r := chi.NewRouter()
	Service{Store: &stubStore{}, WeatherTips: recommendation.Service{Store: wxRecStore{tips}, Weather: snap},
		Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	return r
}

func wxRequest(t *testing.T, snap wxSnapshot, tips []datawizfacts.Fact, question string) (int, Answer, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	wxRouter(snap, tips).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+uuid.NewString()+"/faq/answers/"+question, nil))
	var answer Answer
	if w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &answer) != nil {
		t.Fatal("invalid answer JSON")
	}
	return w.Code, answer, w.Body.Bytes()
}

func wxActiveSnapshot() weather.Snapshot {
	hi := 101.0
	headline := "Heat Advisory issued"
	var days []weather.StoredDay
	for i := 1; i <= 12; i++ {
		days = append(days, weather.StoredDay{Day: weather.Day{Date: fmt.Sprintf("2026-07-%02d", i), Source: weather.SourceObserved, HighF: &hi}, ID: fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", i), Revision: 2})
	}
	return weather.Snapshot{Status: weather.StatusAvailable, Provider: weather.ProviderNWS, LastCompleted: "2026-07-12", Days: days,
		Alerts: []weather.Alert{
			{ID: "11111111-1111-4111-8111-111111111111", Event: "Heat Advisory", Headline: &headline, SourceURL: "https://api.weather.gov/alerts/x", Kind: weather.KindHot, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour)},
			{ID: "22222222-2222-4222-8222-222222222222", Event: "Flood Watch", SourceURL: "https://api.weather.gov/alerts/y"},
		},
		Hot: weather.Run{Days: 12, LastDay: "2026-07-12", Active: true}}
}

func TestFAQWeatherTipsListsAlertsAndActiveTips(t *testing.T) {
	tips := []datawizfacts.Fact{
		wxTip("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "weather_hot", "https://www.pge.com/x"),
		wxTip("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "weather_cold", "https://www.pge.com/cold"),
		wxTip("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "weather_hot", "http://insecure.example/x"),
	}
	code, a, _ := wxRequest(t, wxSnapshot{snap: wxActiveSnapshot()}, tips, "weather_tips")
	if code != 200 || a.QuestionID != "weather_tips" || a.Status != "partial" || a.Text == "" || a.WeatherStatus != "available" || a.WeatherReason != "" || len(a.Findings) != 2 {
		t.Fatalf("answer: %d %+v", code, a)
	}
	alert, tip := a.Findings[0], a.Findings[1]
	if alert.Label != "observation" || alert.Text != "Heat Advisory. Heat Advisory issued" || alert.EvidenceRef[0].Kind != "weather_alert" || alert.EvidenceRef[0].Revision != 1 || alert.SourceURL != "https://api.weather.gov/alerts/x" {
		t.Fatalf("alert: %+v", alert)
	}
	if tip.Label != "hypothesis" || tip.Text != "Tip aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || tip.SourceURL != "https://www.pge.com/x" || len(tip.EvidenceRef) != 1 || tip.EvidenceRef[0].Kind != "weather_alert" {
		t.Fatalf("tip: %+v", tip)
	}
	for _, ref := range a.EvidenceRefs {
		found := false
		for _, f := range a.Findings {
			found = found || slices.Contains(f.EvidenceRef, ref)
		}
		if !found {
			t.Fatalf("top-level ref %+v not in a finding", ref)
		}
	}
}

func TestFAQWeatherTipsRunTipCitesRunDays(t *testing.T) {
	snap := wxActiveSnapshot()
	snap.Alerts = nil
	code, a, _ := wxRequest(t, wxSnapshot{snap: snap}, []datawizfacts.Fact{wxTip("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "weather_hot", "https://www.pge.com/x")}, "weather_tips")
	if code != 200 || len(a.Findings) != 1 || len(a.Findings[0].EvidenceRef) != 12 || a.Findings[0].EvidenceRef[0].Kind != "weather_day" || a.Findings[0].EvidenceRef[0].ID != "00000000-0000-4000-8000-000000000012" || a.Findings[0].EvidenceRef[0].Revision != 2 {
		t.Fatalf("run tip: %d %+v", code, a)
	}
}

func TestFAQWeatherTipsWithNoTriggerIsSupportedAndEmpty(t *testing.T) {
	snap := weather.Snapshot{Status: weather.StatusAvailable, Provider: weather.ProviderNWS}
	code, a, raw := wxRequest(t, wxSnapshot{snap: snap}, []datawizfacts.Fact{wxTip("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "weather_hot", "https://www.pge.com/x")}, "weather_tips")
	if code != 200 || a.Status != "supported" || a.Text != NoWeatherTips || a.WeatherStatus != "available" || len(a.Findings) != 0 || !strings.Contains(string(raw), `"findings":[]`) || !strings.Contains(string(raw), `"evidence_refs":[]`) {
		t.Fatalf("no trigger: %d %s", code, raw)
	}
}

func TestFAQWeatherTipsReportsUnavailableWeather(t *testing.T) {
	for name, tc := range map[string]struct {
		snap   wxSnapshot
		reason string
	}{
		"unsupported": {wxSnapshot{snap: weather.Snapshot{Status: weather.StatusUnavailable, Reason: weather.ReasonUnsupportedLocation}}, "unsupported_location"},
		"invalid":     {wxSnapshot{snap: weather.Snapshot{Status: weather.StatusUnavailable, Reason: weather.ReasonInvalidSourceData}}, "invalid_source_data"},
		"provider":    {wxSnapshot{snap: weather.Snapshot{Status: weather.StatusUnavailable, Reason: weather.ReasonProviderUnavailable}}, "provider_unavailable"},
		"error":       {wxSnapshot{err: errors.New("db down")}, "provider_unavailable"},
	} {
		code, a, _ := wxRequest(t, tc.snap, nil, "weather_tips")
		if code != 200 || a.Status != "insufficient_information" || a.Text != Insufficient || a.WeatherStatus != "missing" || a.WeatherReason != tc.reason || a.NextAction == nil || len(a.Findings) != 0 {
			t.Fatalf("%s: %d %+v", name, code, a)
		}
	}
}

func TestFAQWeatherFailureLeavesOtherIntentsUnchanged(t *testing.T) {
	home := uuid.NewString()
	strip := regexp.MustCompile(`"as_of":"[^"]*"`)
	for _, question := range []string{"high_bill_possible_contributors", "top_waste_opportunities", "reduce_cost", "programs_for_device"} {
		path := "/homes/" + home + "/faq/answers/" + question
		plain := chi.NewRouter()
		Service{Store: &stubStore{}, Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return "owner" }}.Routes(plain)
		var bodies [2]string
		for i, router := range []*chi.Mux{plain, wxRouter(wxSnapshot{err: errors.New("down")}, nil)} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != 200 {
				t.Fatalf("%s: %d", question, w.Code)
			}
			bodies[i] = strip.ReplaceAllString(w.Body.String(), "")
		}
		if bodies[0] != bodies[1] {
			t.Fatalf("%s differs:\n%s\n%s", question, bodies[0], bodies[1])
		}
	}
}

func TestFAQWeatherTipsWithoutServiceReportsUnconfigured(t *testing.T) {
	code, a := faqRequest(t, &stubStore{}, "/homes/"+uuid.NewString()+"/faq/answers/weather_tips")
	if code != 200 || a.Status != "insufficient_information" || a.WeatherStatus != "missing" || a.WeatherReason != "provider_unconfigured" {
		t.Fatalf("unconfigured: %d %+v", code, a)
	}
}
