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
