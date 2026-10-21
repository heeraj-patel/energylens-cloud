package householdenergy

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func splitFixture(t *testing.T, whs []string, specs map[string]splitSpec, hours int) ([][]deviceAllocation, []deviceAllocation) {
	t.Helper()
	return splitFixtureRates(t, whs, specs, hours, true, nil)
}

func splitFixtureRates(t *testing.T, whs []string, specs map[string]splitSpec, hours int, homeTariff bool, national *marginalTariff) ([][]deviceAllocation, []deviceAllocation) {
	t.Helper()
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Duration(hours) * time.Hour)
	now := end.Add(time.Hour)
	fuel := "electricity"
	items := []interval{}
	for i, wh := range whs {
		from := start.Add(time.Duration(i) * 15 * time.Minute)
		items = append(items, interval{Start: from, End: from.Add(15 * time.Minute), Wh: mustDecimal(wh), Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug"})
	}
	segment := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: now.Add(-time.Hour)}
	schedule := selectTariffSchedule(now, []interval{{Start: start, End: end, Wh: big.NewRat(1, 1), Fuel: &fuel}}, []marginalTariff{segment})
	if schedule == nil {
		t.Fatal("tariff not selected")
	}
	buckets := makeBuckets(period{Start: start, End: end}, "hour", items)
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	periodRows := sumDeviceAllocations(wh, nil)
	if !homeTariff {
		schedule = nil
	} else {
		priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
	}
	applyMeasuredSplit(buckets, rows, periodRows, items, items, specs, schedule, national)
	return rows, periodRows
}

func amount(t *testing.T, v any) *big.Rat {
	t.Helper()
	switch x := v.(type) {
	case money:
		return mustDecimal(x.Amount)
	case *quantity:
		return mustDecimal(x.Value)
	}
	t.Fatalf("unexpected %T", v)
	return nil
}

func assertSplit(t *testing.T, a allocation, useful, waste string) {
	t.Helper()
	if amount(t, a.UsefulCost).Cmp(mustDecimal(useful)) != 0 || amount(t, a.WasteCost).Cmp(mustDecimal(waste)) != 0 {
		t.Fatalf("split cost = %+v / %+v, want %s / %s", a.UsefulCost, a.WasteCost, useful, waste)
	}
	sum := new(big.Rat).Add(amount(t, a.UsefulCost), amount(t, a.WasteCost))
	if sum.Cmp(amount(t, a.Cost)) != 0 {
		t.Fatalf("useful+waste %s != measured %+v", sum.FloatString(6), a.Cost)
	}
	energy := new(big.Rat).Add(amount(t, a.UsefulEnergy), amount(t, a.WasteEnergy))
	if energy.Cmp(amount(t, a.Energy)) != 0 {
		t.Fatalf("useful+waste energy %s != %+v", energy.FloatString(12), a.Energy)
	}
}

func TestMeasuredSplitStandbyAndActiveIntervals(t *testing.T) {
	// 0.4 W and 0.6 W are at or below the 0.75 W ceiling; 80 W is active; 0 Wh is neither.
	rows, periodRows := splitFixture(t, []string{"0.1", "0.15", "20", "0"}, map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}, 1)
	assertSplit(t, rows[0][0].Allocation, "0.002000", "0.000025")
	assertSplit(t, periodRows[0].Allocation, "0.002000", "0.000025")
	if rows[0][0].Allocation.Cost.(money).Amount != "0.002025" || rows[0][0].Allocation.UnclassifiedCost != nil {
		t.Fatalf("measured total changed: %+v", rows[0][0].Allocation)
	}
}

func TestMeasuredSplitAcrossBucketsSumsToPeriod(t *testing.T) {
	rows, periodRows := splitFixture(t, []string{"0.1", "0.1", "0.1", "0.1", "30", "0.1", "0.1", "0.1"}, map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}, 2)
	for _, bucketRow := range rows {
		assertSplit(t, bucketRow[0].Allocation, bucketRow[0].Allocation.UsefulCost.(money).Amount, bucketRow[0].Allocation.WasteCost.(money).Amount)
	}
	assertSplit(t, periodRows[0].Allocation, "0.003000", "0.000070")
}

func TestMeasuredSplitAllZeroReadings(t *testing.T) {
	rows, _ := splitFixture(t, []string{"0", "0", "0", "0"}, map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}, 1)
	a := rows[0][0].Allocation
	if amount(t, a.UsefulCost).Sign() != 0 || amount(t, a.WasteCost).Sign() != 0 || amount(t, a.Cost).Sign() != 0 {
		t.Fatalf("zero readings split = %+v", a)
	}
}

func TestMeasuredSplitRecordWithoutStandbyOrFloorIsAllUseful(t *testing.T) {
	rows, periodRows := splitFixture(t, []string{"0.1", "20"}, map[string]splitSpec{"dyson": {}}, 1)
	assertSplit(t, rows[0][0].Allocation, "0.002010", "0.000000")
	assertSplit(t, periodRows[0].Allocation, "0.002010", "0.000000")
	if rows[0][0].Allocation.UnclassifiedCost != nil {
		t.Fatalf("unclassified left: %+v", rows[0][0].Allocation)
	}
}

func TestEstimatedWasteFromRecordOrConvertedFloor(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	parse := func(raw string) resolvedEnergy {
		var r resolvedEnergy
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	near := func(got, want float64) bool { return got > want-1e-9 && got < want+1e-9 }
	standby := parse(`{"values":{"kwh_per_day":0.5,"waste_kwh_per_day":0.009,"waste_basis":"standby"}}`)
	if energy, waste, known := resolutionKWhForPeriod(standby, start, end, 0); !known || !near(energy, 0.5) || !near(waste, 0.009) {
		t.Fatalf("standby waste = %f of %f (%v)", waste, energy, known)
	}
	large := parse(`{"values":{"kwh_per_day":1.32,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.01}}`)
	if _, waste, known := resolutionKWhForPeriod(large, start, end, 0.17); !known || !near(waste, 0.01/0.17) {
		t.Fatalf("large floor waste = %f (%v)", waste, known)
	}
	small := parse(`{"values":{"kwh_per_day":0.274,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.001}}`)
	if _, waste, known := resolutionKWhForPeriod(small, start, end, 0.17); !known || !near(waste, 0.001/0.17) {
		t.Fatalf("small floor waste = %f (%v)", waste, known)
	}
	router := parse(`{"values":{"kwh_per_day":0.0008,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.001}}`)
	if _, waste, known := resolutionKWhForPeriod(router, start, end, 0.17); !known || !near(waste, 0.0008) {
		t.Fatalf("capped floor waste = %f (%v)", waste, known)
	}
	if _, _, known := resolutionKWhForPeriod(large, start, end, 0); known {
		t.Fatal("floor waste without a rate must be unknown")
	}
	v1 := parse(`{"values":{"kwh_per_day":0.5}}`)
	if _, waste, known := resolutionKWhForPeriod(v1, start, end, 0.17); known || waste != 0 {
		t.Fatalf("v1 waste without a figure = %f (%v)", waste, known)
	}
}

func nationalSchedule(rate string) *marginalTariff {
	r := mustDecimal(rate)
	return &marginalTariff{Currency: "USD", Rate: r, Segments: []marginalTariff{{Currency: "USD", Rate: r}}}
}

func requireWaste(t *testing.T, a allocation, wantWasteWh float64) {
	t.Helper()
	waste, _ := amount(t, a.WasteEnergy).Float64()
	if waste*1000 < wantWasteWh-1e-6 || waste*1000 > wantWasteWh+1e-6 {
		t.Fatalf("waste = %f Wh, want %f", waste*1000, wantWasteWh)
	}
	sum := new(big.Rat).Add(amount(t, a.UsefulEnergy), amount(t, a.WasteEnergy))
	if sum.Cmp(amount(t, a.Energy)) != 0 {
		t.Fatalf("useful+waste %s != total %+v", sum.FloatString(12), a.Energy)
	}
}

func TestMeasuredSplitFloorWithHomeTariff(t *testing.T) {
	// $0.01/day at $0.10/kWh is 100 Wh/day, so one covered hour carries 100/24 Wh.
	rows, periodRows := splitFixture(t, []string{"20", "20", "20", "20"}, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, 1)
	a := rows[0][0].Allocation
	requireWaste(t, a, 100.0/24)
	if a.WasteBasis != "floor" || a.RateBasis != "home_tariff" || a.Energy.Value != "0.080000000000" {
		t.Fatalf("floor row = %+v", a)
	}
	assertSplit(t, a, amount(t, a.UsefulCost).FloatString(6), amount(t, a.WasteCost).FloatString(6))
	if got := amount(t, a.WasteCost).FloatString(6); got != "0.000417" {
		t.Fatalf("waste cost = %s", got)
	}
	requireWaste(t, periodRows[0].Allocation, 100.0/24)
}

func TestMeasuredSplitFloorCappedAtTotal(t *testing.T) {
	rows, _ := splitFixture(t, []string{"0.1", "0.1", "0.1", "0.1"}, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, 1)
	a := rows[0][0].Allocation
	requireWaste(t, a, 0.4)
	if amount(t, a.UsefulEnergy).Sign() != 0 {
		t.Fatalf("useful after cap = %+v", a.UsefulEnergy)
	}
}

func TestMeasuredSplitFloorWithoutTariffUsesNationalRate(t *testing.T) {
	// $0.01/day at $0.20/kWh is 50 Wh/day.
	rows, periodRows := splitFixtureRates(t, []string{"20", "20", "20", "20"}, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, 1, false, nationalSchedule("0.2"))
	a := rows[0][0].Allocation
	requireWaste(t, a, 50.0/24)
	if a.WasteBasis != "floor" || a.RateBasis != "us_average" || a.Cost != nil {
		t.Fatalf("national floor row = %+v", a)
	}
	requireWaste(t, periodRows[0].Allocation, 50.0/24)
	rows, _ = splitFixtureRates(t, []string{"20"}, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, 1, false, nil)
	if hasSplit(rows[0][0].Allocation) {
		t.Fatalf("split without any rate: %+v", rows[0][0].Allocation)
	}
}

func TestMeasuredSplitFloorAllZeroReadings(t *testing.T) {
	rows, _ := splitFixture(t, []string{"0", "0", "0", "0"}, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, 1)
	a := rows[0][0].Allocation
	if amount(t, a.UsefulCost).Sign() != 0 || amount(t, a.WasteCost).Sign() != 0 || amount(t, a.Cost).Sign() != 0 {
		t.Fatalf("zero readings floor split = %+v", a)
	}
}

func TestMeasuredSplitWithoutRecordStaysUnsplit(t *testing.T) {
	rows, periodRows := splitFixture(t, []string{"0.1", "20"}, map[string]splitSpec{}, 1)
	if hasSplit(rows[0][0].Allocation) || hasSplit(periodRows[0].Allocation) {
		t.Fatalf("row without a record was split: %+v", rows[0][0].Allocation)
	}
}

func TestMeasuredSplitKeepsExistingSplit(t *testing.T) {
	waste := &quantity{Value: "0.5", Unit: "kWh"}
	rows := [][]deviceAllocation{{{ApplianceID: "dyson", Allocation: allocation{Energy: &quantity{Value: "1", Unit: "kWh"}, WasteEnergy: waste}}}}
	buckets := []bucket{{Start: time.Now(), End: time.Now().Add(time.Hour)}}
	applyMeasuredSplit(buckets, rows, nil, nil, nil, map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}, nil, nil)
	if rows[0][0].Allocation.WasteEnergy != waste || rows[0][0].Allocation.UsefulEnergy != nil {
		t.Fatalf("existing split overwritten: %+v", rows[0][0].Allocation)
	}
}

func TestMeteredSpecYieldsOnlyMeasuredSplitRow(t *testing.T) {
	var spec resolvedEnergy
	if err := json.Unmarshal([]byte(`{"status":"resolved","device_kind":"real_metering","values":{"kwh_per_day":0.5,"standby_watts":0.5}}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.estimable() {
		t.Fatal("metered spec must not produce an estimated row")
	}
	rows, _ := splitFixture(t, []string{"0.1", "20"}, map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}, 1)
	a := rows[0][0].Allocation
	if a.EvidenceBasis == "estimated" || a.Cost.(money).Amount != "0.002010" {
		t.Fatalf("metered row = %+v", a)
	}
	assertSplit(t, a, "0.002000", "0.000010")
}

func TestNationalRatePricesMeteredRowsAndSplit(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	fuel := "electricity"
	items := []interval{{Start: start, End: start.Add(15 * time.Minute), Wh: mustDecimal("20"), Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug"}}
	national := NationalRate(func(context.Context) (float64, error) { return 0.2, nil })
	schedule := national.scheduleFor(context.Background(), items)
	if schedule == nil || schedule.nationalBasis() != "us_average" {
		t.Fatalf("national schedule = %+v", schedule)
	}
	if national.scheduleFor(context.Background(), nil) != nil {
		t.Fatal("empty items were priced")
	}
	unavailable := []interval{{Start: start, End: start.Add(time.Hour), Unavailable: true, Fuel: &fuel}}
	if national.scheduleFor(context.Background(), unavailable) != nil {
		t.Fatal("unavailable interval was priced")
	}
	if NationalRate(nil).scheduleFor(context.Background(), items) != nil {
		t.Fatal("nil national rate priced the home")
	}
	buckets := makeBuckets(period{Start: start, End: start.Add(time.Hour)}, "hour", items)
	total := priceEffectiveBuckets(buckets, items, *schedule)
	if total == nil || total.Amount != "0.004000" {
		t.Fatalf("whole-home cost = %+v", total)
	}
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	periodRows := sumDeviceAllocations(wh, nil)
	priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
	markNationalRate(schedule, rows, periodRows)
	applyMeasuredSplit(buckets, rows, periodRows, items, items, map[string]splitSpec{"dyson": {FloorUSDPerDay: 0.01}}, schedule, nil)
	for _, a := range []allocation{rows[0][0].Allocation, periodRows[0].Allocation} {
		if a.Cost.(money).Amount != "0.004000" || a.RateBasis != "us_average" || a.WasteBasis != "floor" {
			t.Fatalf("metered national row = %+v", a)
		}
		// $0.01/day at $0.20/kWh is 50 Wh/day; 15 covered minutes is 0.52 Wh.
		assertSplit(t, a, "0.003896", "0.000104")
	}
}

func TestSpecForFuelKeepsGasOffFloorAndTypicalStandby(t *testing.T) {
	in := splitSpec{StandbyWatts: 3, FloorUSDPerDay: 0.01}
	if got := specForFuel("electricity", "category_typical", in); got != in {
		t.Fatalf("electric changed: %+v", got)
	}
	if got := specForFuel("natural_gas", "category_typical", in); got != (splitSpec{}) {
		t.Fatalf("gas typical spec: %+v", got)
	}
	if got := specForFuel("natural_gas", "catalog", in); got != (splitSpec{StandbyWatts: 3}) {
		t.Fatalf("gas catalog spec: %+v", got)
	}
}

func TestSpecForFuelTreatsEmptyFuelAsElectricity(t *testing.T) {
	in := splitSpec{StandbyWatts: 3, FloorUSDPerDay: 0.01}
	if got := specForFuel("", "category_typical", in); got != in {
		t.Fatalf("empty fuel treated as gas: %+v", got)
	}
}
