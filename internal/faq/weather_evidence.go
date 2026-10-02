package faq

import (
	"fmt"
	"math"
	"time"

	"energylens.local/cloud/internal/weather"
	"github.com/google/uuid"
)

// WeatherFinding compares the last seven completed local days with the seven
// days immediately before them. A temperature change is context for possible
// heating or cooling demand, never evidence of a particular bill or device use.
func WeatherFinding(home string, locationRevision int64, observations []weather.Observation, now time.Time) *Finding {
	if _, err := uuid.Parse(home); err != nil || locationRevision < 1 || len(observations) != 14 {
		return nil
	}
	provider, cell, timezone := observations[0].Provider, observations[0].CellKey, observations[0].Timezone
	if provider == "" || cell == "" || timezone == "" {
		return nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return nil
	}
	localNow := now.In(loc)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC)
	byDay := make(map[string]weather.Observation, 14)
	var expiry time.Time
	for _, o := range observations {
		if o.Provider != provider || o.CellKey != cell || o.Timezone != timezone || o.SourceRevision == "" || math.IsNaN(o.MeanTemperatureC) || math.IsInf(o.MeanTemperatureC, 0) || o.MeanTemperatureC < -100 || o.MeanTemperatureC > 70 || !o.RetrievedAt.Before(o.ExpiresAt) || o.RetrievedAt.After(now) || !now.Before(o.ExpiresAt) {
			return nil
		}
		if _, err := uuid.Parse(o.ID); err != nil {
			return nil
		}
		if _, exists := byDay[o.LocalDate]; exists {
			return nil
		}
		byDay[o.LocalDate] = o
		if expiry.IsZero() || o.ExpiresAt.Before(expiry) {
			expiry = o.ExpiresAt
		}
	}
	var baseline, recent float64
	refs := []Evidence{{ID: home, Kind: "home_location", Revision: locationRevision}}
	for ago := 14; ago >= 1; ago-- {
		day := today.AddDate(0, 0, -ago).Format("2006-01-02")
		o, ok := byDay[day]
		if !ok {
			return nil
		}
		if ago > 7 {
			baseline += o.MeanTemperatureC
		} else {
			recent += o.MeanTemperatureC
		}
		refs = append(refs, Evidence{ID: o.ID, Kind: "weather_observation", Revision: 1})
	}
	baseline /= 7
	recent /= 7
	diff := recent - baseline
	if math.Abs(diff) < 3 {
		return nil
	}
	condition, demand := "warmer", "cooling"
	if diff < 0 {
		condition, demand = "colder", "heating"
	}
	text := fmt.Sprintf("Weather context: %s to %s averaged %.1f°C, %s than the baseline %s to %s (%.1f°C). Coverage: 7 of 7 completed days in each period. %s demand may be higher depending on equipment and settings; this does not establish a change in home energy use or a bill cause. Weather evidence expires %s.",
		today.AddDate(0, 0, -7).Format("2006-01-02"), today.AddDate(0, 0, -1).Format("2006-01-02"), recent, condition,
		today.AddDate(0, 0, -14).Format("2006-01-02"), today.AddDate(0, 0, -8).Format("2006-01-02"), baseline, demand, expiry.UTC().Format(time.RFC3339))
	return &Finding{Label: "observation", Text: text, EvidenceRef: refs}
}
