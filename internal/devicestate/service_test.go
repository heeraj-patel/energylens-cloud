package devicestate

import (
	"testing"
	"time"
)

func TestCurrentStateRequiresFreshMeasuredPower(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-freshFor - time.Second)
	positive, zero := 12.5, 0.0
	for _, tc := range []struct {
		name     string
		observed *time.Time
		watts    *float64
		want     string
	}{
		{"fresh positive", &fresh, &positive, "active"},
		{"fresh zero", &fresh, &zero, "idle"},
		{"stale positive", &stale, &positive, "offline"},
		{"missing watt", &fresh, nil, "offline"},
		{"unreachable", nil, nil, "offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.observed, tc.watts, now); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}
