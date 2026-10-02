package outcomes

import (
	"math"
	"testing"
	"time"
)

func fullBuckets(action time.Time, beforeWh, afterWh float64) []Bucket {
	out := make([]Bucket, 0, hours*2)
	for i := -hours; i < hours; i++ {
		wh := beforeWh
		if i >= 0 {
			wh = afterWh
		}
		out = append(out, Bucket{At: action.Add(time.Duration(i) * time.Hour), Wh: wh, Covered: 3600, Status: "complete", Allocation: "exact"})
	}
	return out
}

func TestOutcomeRequiresComparableMeasuredWindows(t *testing.T) {
	at := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	a := Action{OccurredAt: at, AttachedFrom: at.Add(-8 * 24 * time.Hour), ComparableUse: true, ComparableWeather: true, Confounders: []string{}}
	base := fullBuckets(at, 10, 8)
	check := func(name string, rows []Bucket, action Action, now time.Time, status, reason string) {
		t.Helper()
		o := Evaluate(action, rows, now)
		if o.Status != status {
			t.Fatalf("%s status=%s reasons=%v", name, o.Status, o.Reasons)
		}
		if reason != "" {
			found := false
			for _, r := range o.Reasons {
				if r == reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s missing reason %s: %v", name, reason, o.Reasons)
			}
		}
		if status != "observed-change" && o.DeltaWh != nil {
			t.Fatalf("%s disclosed delta", name)
		}
	}
	check("collecting", base, a, at.Add(6*24*time.Hour), "collecting", "follow_up_window_open")
	o := Evaluate(a, base, at.Add(8*24*time.Hour))
	if o.Status != "observed-change" || o.DeltaWh == nil || math.Abs(*o.DeltaWh-(-336)) > 0.001 || o.Before.EnergyWh == nil || o.After.EnergyWh == nil {
		t.Fatalf("unexpected qualified outcome: %+v", o)
	}
	missing := append([]Bucket{}, base[:hours+1]...)
	missing = append(missing, base[hours+2:]...)
	check("missing hour", missing, a, at.Add(8*24*time.Hour), "inconclusive", "insufficient_measured_coverage")
	gap := append([]Bucket{}, base...)
	gap[hours+3].GapCount = 1
	check("known gap", gap, a, at.Add(8*24*time.Hour), "inconclusive", "insufficient_measured_coverage")
	low := append([]Bucket{}, base...)
	low[0].Covered = 600
	check("low coverage", low, a, at.Add(8*24*time.Hour), "inconclusive", "insufficient_measured_coverage")
	allocated := append([]Bucket{}, base...)
	allocated[hours+3].Allocation = "duration_proportional"
	check("allocated measurement", allocated, a, at.Add(8*24*time.Hour), "inconclusive", "allocated_measurement")
	a.ComparableWeather = false
	check("weather", base, a, at.Add(8*24*time.Hour), "inconclusive", "weather_not_confirmed_comparable")
	a.ComparableWeather = true
	a.Confounders = []string{"use_changed"}
	check("confounder", base, a, at.Add(8*24*time.Hour), "inconclusive", "reported_confounders")
	a.Confounders = nil
	until := at.Add(2 * 24 * time.Hour)
	a.AttachedUntil = &until
	check("moved meter", base, a, at.Add(8*24*time.Hour), "inconclusive", "attachment_changed")
	a.AttachedUntil = nil
	check("no clear difference", fullBuckets(at, 10, 9.7), a, at.Add(8*24*time.Hour), "inconclusive", "no_clear_change")
}
