package energyrate

import (
	"context"
	"errors"
	"testing"
	"time"
)

type failingRateFetcher struct{}

func (failingRateFetcher) LatestResidentialPrice(context.Context, string) (Rate, error) {
	return Rate{}, errors.New("EIA unavailable")
}

func TestCachedEIARateKeepsSourceTimeAndExpires(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	cache := NewMemCache()
	period := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	fetched := now.Add(-48 * time.Hour)
	if err := cache.Put(context.Background(), Rate{Region: "TX", USDPerKWh: .17, Period: period, FetchedAt: fetched}); err != nil {
		t.Fatal(err)
	}
	service := NewService(Options{Cache: cache, CacheTTL: 24 * time.Hour, Fetcher: failingRateFetcher{}, HasKey: true, Now: func() time.Time { return now }})
	rate, err := service.Resolve(context.Background(), "US", "78732")
	if err != nil || rate.FetchedAt == nil || !rate.FetchedAt.Equal(fetched) || !rate.IsStale || rate.IsStatic {
		t.Fatalf("stale source provenance lost: rate=%+v err=%v", rate, err)
	}
	now = now.Add(MaxSourceAge)
	_, stale, ok := service.regionRate(context.Background(), "TX")
	if ok || stale {
		t.Fatal("expired EIA source remained usable")
	}
}

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
	if !rate.IsStatic || rate.FetchedAt != nil || rate.IsStale {
		t.Fatalf("published fallback has invented fetch provenance: %+v", rate)
	}
}
