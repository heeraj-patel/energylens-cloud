package programs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestApplicationDeadlineIncludesHomeLocalDayAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		day, utc string
	}{
		{"2026-03-08", "2026-03-09T05:00:00Z"},
		{"2026-11-01", "2026-11-02T06:00:00Z"},
	} {
		day, err := time.Parse("2006-01-02", tc.day)
		if err != nil {
			t.Fatal(err)
		}
		deadline := applicationDeadline(day, zone)
		if got := deadline.UTC().Format(time.RFC3339); got != tc.utc {
			t.Errorf("%s deadline = %s, want %s", tc.day, got, tc.utc)
		}
		instant := deadline.UTC().Format(time.RFC3339)
		wire, err := json.Marshal(OwnerProgram{AvailabilityExpiresAt: &instant})
		if err != nil || !strings.Contains(string(wire), `"availability_expires_at":"`+tc.utc+`"`) {
			t.Errorf("%s deadline wire = %s, error %v", tc.day, wire, err)
		}
	}
}
