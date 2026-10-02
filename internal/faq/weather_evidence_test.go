package faq

import (
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/weather"
	"github.com/google/uuid"
)

func weatherFixture() (string, []weather.Observation, time.Time) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := uuid.NewString()
	var observations []weather.Observation
	for ago := 14; ago >= 1; ago-- {
		mean := 25.0
		if ago <= 7 {
			mean = 18
		}
		observations = append(observations, weather.Observation{ID: uuid.NewString(), Provider: "test-source", CellKey: "US:787:America/Chicago", Timezone: "America/Chicago", LocalDate: now.AddDate(0, 0, -ago).Format("2006-01-02"), MeanTemperatureC: mean, SourceRevision: "source-1", RetrievedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
	}
	return home, observations, now
}

func TestWeatherFindingRequiresCompleteFreshCoherentBaseline(t *testing.T) {
	home, observations, now := weatherFixture()
	f := WeatherFinding(home, 2, observations, now)
	if f == nil || len(f.EvidenceRef) != 15 || !strings.Contains(f.Text, "Coverage: 7 of 7") || !strings.Contains(f.Text, "baseline 2026-09-17 to 2026-09-23") || !strings.Contains(f.Text, "2026-10-01T13:00:00Z") || !strings.Contains(f.Text, "does not establish a change in home energy use or a bill cause") {
		t.Fatalf("bounded weather context: %+v", f)
	}
	for name, mutate := range map[string]func([]weather.Observation){
		"missing day":      func(v []weather.Observation) { v[0].LocalDate = "2026-09-30" },
		"expired":          func(v []weather.Observation) { v[0].ExpiresAt = now },
		"mixed provider":   func(v []weather.Observation) { v[0].Provider = "other" },
		"future retrieval": func(v []weather.Observation) { v[0].RetrievedAt = now.Add(time.Minute) },
		"invalid source":   func(v []weather.Observation) { v[0].SourceRevision = "" },
	} {
		v := append([]weather.Observation(nil), observations...)
		mutate(v)
		if got := WeatherFinding(home, 2, v, now); got != nil {
			t.Errorf("%s escaped: %+v", name, got)
		}
	}
	if got := WeatherFinding(home, 0, observations, now); got != nil {
		t.Fatal("missing location revision escaped")
	}
	for i := range observations {
		if i < 7 {
			observations[i].MeanTemperatureC = 20
		} else {
			observations[i].MeanTemperatureC = 18
		}
	}
	if got := WeatherFinding(home, 2, observations, now); got != nil {
		t.Fatalf("small change escaped: %+v", got)
	}
}
