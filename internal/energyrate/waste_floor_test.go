package energyrate

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestWasteFloorKWhConvertsAndCaps(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if got := WasteFloorKWh(0.01, 0.17, 1.32); !near(got, 0.01/0.17) {
		t.Fatalf("large floor = %f", got)
	}
	if got := WasteFloorKWh(0.001, 0.17, 0.274); !near(got, 0.001/0.17) {
		t.Fatalf("small floor = %f", got)
	}
	if got := WasteFloorKWh(0.001, 0.17, 0.0008); !near(got, 0.0008) {
		t.Fatalf("capped floor = %f", got)
	}
	if WasteFloorKWh(0.01, 0, 1) != 0 || WasteFloorKWh(0, 0.17, 1) != 0 || WasteFloorKWh(0.01, 0.17, 0) != 0 {
		t.Fatal("floor without rate, dollars or usage must be zero")
	}
}

func TestResolutionWasteKWh(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name            string
		daily, rate     float64
		reported, floor *float64
		want            float64
		known           bool
	}{
		{"reported overrides floor", 1, .2, ptr(.3), ptr(.01), .3, true},
		{"reported zero overrides floor", 1, .2, ptr(0), ptr(.01), 0, true},
		{"reported without rate", 1, 0, ptr(.3), nil, .3, true},
		{"large floor", 1.32, .17, nil, ptr(.01), .01 / .17, true},
		{"small floor", .274, .17, nil, ptr(.001), .001 / .17, true},
		{"floor capped", .0008, .17, nil, ptr(.001), .0008, true},
		{"floor without rate", 1, 0, nil, ptr(.01), 0, false},
		{"floor with negative rate", 1, -.2, nil, ptr(.01), 0, false},
		{"no fields", 1, .2, nil, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := ResolutionWasteKWh(tc.daily, tc.rate, tc.reported, tc.floor)
			if known != tc.known || math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("got (%f, %v), want (%f, %v)", got, known, tc.want, tc.known)
			}
		})
	}
}

func TestNationalUSDPerKWhIgnoresStateAndUsesNationalRow(t *testing.T) {
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	cache := NewMemCache()
	period := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	for _, rate := range []Rate{
		{Region: "TX", USDPerKWh: .14, Period: period, FetchedAt: now},
		{Region: NationalRegion, USDPerKWh: .21, Period: period, FetchedAt: now},
	} {
		if err := cache.Put(context.Background(), rate); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewService(Options{Cache: cache, Now: func() time.Time { return now }}).NationalUSDPerKWh(context.Background())
	if err != nil || got != .21 {
		t.Fatalf("national rate = %f, %v", got, err)
	}
	if _, err = NewService(Options{Cache: NewMemCache()}).NationalUSDPerKWh(context.Background()); err == nil {
		t.Fatal("national rate without cache, key or static value must fail")
	}
}

func TestNationalUSDPerKWhFallsBackToStaticWhenEIAUnavailable(t *testing.T) {
	static := StaticRate{USDPerKWh: 0.1831, AsOf: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)}
	got, err := NewService(Options{Cache: NewMemCache(), Static: static}).NationalUSDPerKWh(context.Background())
	if err != nil || got != 0.1831 {
		t.Fatalf("national static rate = %f, %v", got, err)
	}
}
