package energyrate

import (
	"context"
	"testing"
	"time"
)

func TestPilotTexasPublishedFallback(t *testing.T) {
	date := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	service := NewService(Options{StaticStates: map[string]StaticRate{
		"TX": {USDPerKWh: 0.1588, AsOf: date, SourceURL: "https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a"},
	}})
	if got := StateForZIP("78732"); got != "TX" {
		t.Fatalf("ZIP 78732 mapped to %q", got)
	}
	rate, err := service.Resolve(context.Background(), "US", "78732")
	if err != nil {
		t.Fatal(err)
	}
	if rate.RateUSDPerKWh != 0.1588 || rate.Source != SourceState || rate.AsOf != "2026-07-01" || rate.SourceURL == "" {
		t.Fatalf("unexpected rate provenance: %+v", rate)
	}
	if rate.Region == nil || *rate.Region != "TX" {
		t.Fatalf("unexpected region: %+v", rate.Region)
	}
}
