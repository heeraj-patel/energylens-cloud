package energyrate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestExpiredStateRateFallsBackToFreshNationalRate(t *testing.T) {
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	cache := NewMemCache()
	period := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	for _, rate := range []Rate{
		{Region: "TX", USDPerKWh: .14, Period: period, FetchedAt: now.Add(-MaxSourceAge - time.Nanosecond)},
		{Region: NationalRegion, USDPerKWh: .21, Period: period, FetchedAt: now.Add(-time.Hour)},
	} {
		if err := cache.Put(context.Background(), rate); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(Options{Cache: cache, Now: func() time.Time { return now }})
	got, err := service.Resolve(context.Background(), "US", "78732")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceNational || got.RateUSDPerKWh != .21 || got.Region != nil || got.FetchedAt == nil || !got.FetchedAt.Equal(now.Add(-time.Hour)) || got.IsStale {
		t.Fatalf("expired state evidence did not fall through to fresh national rate: %+v", got)
	}
}

func TestUnknownUSZIPPrefixUsesNationalStaticRate(t *testing.T) {
	service := NewService(Options{Static: StaticRate{
		USDPerKWh: .19,
		AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		SourceURL: "https://example.test/national-rate",
	}})
	got, err := service.Resolve(context.Background(), "US", "00601") // Puerto Rico: no state EIA row.
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceNational || got.RateUSDPerKWh != .19 || got.Region != nil || !got.IsStatic || got.SourceURL == "" {
		t.Fatalf("unknown US prefix did not use national static fallback: %+v", got)
	}
}

func TestRatesHTTPDistinguishesInvalidPostalFromUnsupportedCountry(t *testing.T) {
	service := NewService(Options{Static: StaticRate{
		USDPerKWh: .19,
		AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
	}})
	router := chi.NewRouter()
	service.Routes(router)

	for _, tc := range []struct {
		name, query, wantCode string
		status                int
	}{
		{name: "malformed US ZIP", query: "country=US&postal_code=7873A", wantCode: "invalid_postal_code", status: http.StatusBadRequest},
		{name: "unsupported country", query: "country=CA&postal_code=H2X1Y4", wantCode: "rate_unavailable", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/energy/rates?"+tc.query, nil)
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("response = %d %s; want %d code %s", recorder.Code, recorder.Body.String(), tc.status, tc.wantCode)
			}
		})
	}
}
