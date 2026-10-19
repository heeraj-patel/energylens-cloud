package householdenergy

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func splitFixture(t *testing.T, whs []string, specs map[string]float64, hours int) ([][]deviceAllocation, []deviceAllocation) {
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
	priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
	applyMeasuredSplit(buckets, rows, periodRows, items, items, specs, schedule)
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
	rows, periodRows := splitFixture(t, []string{"0.1", "0.15", "20", "0"}, map[string]float64{"dyson": 0.5}, 1)
	assertSplit(t, rows[0][0].Allocation, "0.002000", "0.000025")
	assertSplit(t, periodRows[0].Allocation, "0.002000", "0.000025")
	if rows[0][0].Allocation.Cost.(money).Amount != "0.002025" || rows[0][0].Allocation.UnclassifiedCost != nil {
		t.Fatalf("measured total changed: %+v", rows[0][0].Allocation)
	}
}

func TestMeasuredSplitAcrossBucketsSumsToPeriod(t *testing.T) {
	rows, periodRows := splitFixture(t, []string{"0.1", "0.1", "0.1", "0.1", "30", "0.1", "0.1", "0.1"}, map[string]float64{"dyson": 0.5}, 2)
	for _, bucketRow := range rows {
		assertSplit(t, bucketRow[0].Allocation, bucketRow[0].Allocation.UsefulCost.(money).Amount, bucketRow[0].Allocation.WasteCost.(money).Amount)
	}
	assertSplit(t, periodRows[0].Allocation, "0.003000", "0.000070")
}

func TestMeasuredSplitAllZeroReadings(t *testing.T) {
	rows, _ := splitFixture(t, []string{"0", "0", "0", "0"}, map[string]float64{"dyson": 0.5}, 1)
	a := rows[0][0].Allocation
	if amount(t, a.UsefulCost).Sign() != 0 || amount(t, a.WasteCost).Sign() != 0 || amount(t, a.Cost).Sign() != 0 {
		t.Fatalf("zero readings split = %+v", a)
	}
}

func TestMeasuredSplitWithoutSpecIsAllUseful(t *testing.T) {
	rows, periodRows := splitFixture(t, []string{"0.1", "20"}, map[string]float64{}, 1)
	assertSplit(t, rows[0][0].Allocation, "0.002010", "0.000000")
	assertSplit(t, periodRows[0].Allocation, "0.002010", "0.000000")
	if rows[0][0].Allocation.UnclassifiedCost != nil {
		t.Fatalf("unclassified left: %+v", rows[0][0].Allocation)
	}
}

func TestEstimatedWasteUsesDatawizStandby(t *testing.T) {
	var withStandby, without resolvedEnergy
	if err := json.Unmarshal([]byte(`{"values":{"kwh_per_day":0.5,"standby_watts":0.5,"usage_basis":{"kind":"hours_per_week","value":42,"from":"household"}}}`), &withStandby); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"values":{"kwh_per_day":0.5}}`), &without); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	energy, waste := resolutionKWhForPeriod(withStandby, start, start.AddDate(0, 0, 1))
	if energy < 0.4999 || energy > 0.5001 || waste < 0.00899 || waste > 0.00901 {
		t.Fatalf("standby waste = %f of %f", waste, energy)
	}
	if _, waste = resolutionKWhForPeriod(without, start, start.AddDate(0, 0, 1)); waste != 0 {
		t.Fatalf("waste without standby = %f", waste)
	}
}

func TestMeasuredSplitKeepsExistingSplit(t *testing.T) {
	waste := &quantity{Value: "0.5", Unit: "kWh"}
	rows := [][]deviceAllocation{{{ApplianceID: "dyson", Allocation: allocation{Energy: &quantity{Value: "1", Unit: "kWh"}, WasteEnergy: waste}}}}
	buckets := []bucket{{Start: time.Now(), End: time.Now().Add(time.Hour)}}
	applyMeasuredSplit(buckets, rows, nil, nil, nil, map[string]float64{"dyson": 0.5}, nil)
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
	rows, _ := splitFixture(t, []string{"0.1", "20"}, map[string]float64{"dyson": *spec.Values.StandbyWatts}, 1)
	a := rows[0][0].Allocation
	if a.EvidenceBasis == "estimated" || a.Cost.(money).Amount != "0.002010" {
		t.Fatalf("metered row = %+v", a)
	}
	assertSplit(t, a, "0.002000", "0.000010")
}
