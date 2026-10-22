package householdenergy

import (
	tariffpkg "energylens.local/cloud/internal/tariff"
	"math/big"
	"testing"
	"time"
)

func TestEffectiveTariffCoverageAndExactSplit(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Hour)
	middle, end := start.Add(30*time.Minute), start.Add(time.Hour)
	fuel := "electricity"
	items := []interval{{Start: start, End: end, Wh: big.NewRat(1000, 1), Fuel: &fuel, ApplianceID: "appliance", SensorKind: "plug"}}
	fresh := now.Add(-24 * time.Hour)
	first := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), To: &middle, SourceAsOf: fresh}
	second := marginalTariff{Rate: big.NewRat(2, 10), Currency: "USD", From: middle, SourceAsOf: fresh}
	schedule := selectTariffSchedule(now, items, []marginalTariff{first, second})
	if schedule == nil {
		t.Fatal("adjacent effective revisions did not cover the interval")
	}
	for _, granularity := range []string{"hour", "day"} {
		p := period{Start: start, End: end}
		buckets := makeBuckets(p, granularity, items)
		cost := priceEffectiveBuckets(buckets, items, *schedule)
		if cost == nil || cost.Amount != "0.150000" || buckets[0].Allocation.Cost.(money).Amount != "0.150000" {
			t.Fatalf("%s price = %+v, buckets=%+v", granularity, cost, buckets)
		}
		rows, wh := attributeDeviceBuckets(buckets, items, nil)
		periodRows := sumDeviceAllocations(wh, nil)
		priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
		if len(rows[0]) != 1 || rows[0][0].Allocation.Cost.(money).Amount != "0.150000" || len(periodRows) != 1 || periodRows[0].Allocation.Cost.(money).Amount != "0.150000" {
			t.Fatalf("%s appliance price diverged: bucket=%+v period=%+v", granularity, rows, periodRows)
		}
	}
	for _, tc := range []struct {
		name     string
		segments []marginalTariff
	}{
		{"gap", []marginalTariff{first}},
		{"stale", []marginalTariff{first, {Rate: second.Rate, Currency: "USD", From: middle, SourceAsOf: now.Add(-tariffpkg.ElectricityMaxAge)}}},
		{"currency conflict", []marginalTariff{first, {Rate: second.Rate, Currency: "EUR", From: middle, SourceAsOf: fresh}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectTariffSchedule(now, items, tc.segments); got != nil {
				t.Fatalf("invalid schedule was priced: %+v", got)
			}
		})
	}
}

func TestEffectiveTariffOverlapUsesRankThenLatest(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Hour)
	middle, end := start.Add(30*time.Minute), start.Add(time.Hour)
	fuel := "electricity"
	items := []interval{{Start: start, End: end, Wh: big.NewRat(1000, 1), Fuel: &fuel}}
	fresh := now.Add(-24 * time.Hour)
	userTariff := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: fresh, Revision: 1}
	laterDefault := marginalTariff{Rate: big.NewRat(2, 10), Currency: "USD", From: middle, SourceAsOf: fresh, Revision: 2, Rank: 1}
	laterUser := marginalTariff{Rate: big.NewRat(3, 10), Currency: "USD", From: middle, SourceAsOf: fresh, Revision: 3}
	for _, tc := range []struct {
		name     string
		segments []marginalTariff
		want     string
	}{
		{"user beats a later default", []marginalTariff{userTariff, laterDefault}, "0.100000"},
		{"latest of one rank wins", []marginalTariff{userTariff, laterDefault, laterUser}, "0.200000"},
		{"stale higher rank is skipped", []marginalTariff{{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: now.Add(-tariffpkg.ElectricityMaxAge)},
			{Rate: big.NewRat(2, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: fresh, Rank: 1}}, "0.200000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule := selectTariffSchedule(now, items, tc.segments)
			if schedule == nil {
				t.Fatal("overlapping fresh revisions were not priced")
			}
			if got := priceIntervalRange(items[0], start, end, *schedule).FloatString(6); got != tc.want {
				t.Fatalf("price %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEffectiveTariffSubCentConservedAcrossBuckets(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	fuel := "electricity"
	items := []interval{}
	for i := 0; i < 24; i++ {
		items = append(items, interval{Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i+1) * time.Hour), Wh: big.NewRat(1, 1), Fuel: &fuel})
	}
	rate := marginalTariff{Rate: big.NewRat(1, 100), Currency: "USD", From: start, SourceAsOf: start}
	schedule := marginalTariff{Currency: "USD", Segments: []marginalTariff{rate}}
	p := period{Start: start, End: start.Add(24 * time.Hour)}
	hourly, daily := makeBuckets(p, "hour", items), makeBuckets(p, "day", items)
	hourlyCost := priceEffectiveBuckets(hourly, items, schedule)
	dailyCost := priceEffectiveBuckets(daily, items, schedule)
	if hourlyCost == nil || dailyCost == nil || hourlyCost.Amount != "0.000240" || hourlyCost.Amount != dailyCost.Amount {
		t.Fatalf("sub-cent totals differ: hour=%+v day=%+v", hourlyCost, dailyCost)
	}
	sum := new(big.Rat)
	for _, b := range hourly {
		sum.Add(sum, mustDecimal(b.Allocation.Cost.(money).Amount))
	}
	if sum.Cmp(mustDecimal(hourlyCost.Amount)) != 0 {
		t.Fatalf("hourly display sum %s differs from total %s", sum, hourlyCost.Amount)
	}
}
