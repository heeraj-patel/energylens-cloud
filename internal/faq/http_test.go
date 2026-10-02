package faq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
