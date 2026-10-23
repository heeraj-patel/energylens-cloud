package householdenergy

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"
)

func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNightSpansFollowLocalWallClockAcrossDST(t *testing.T) {
	loc := nyLoc(t)
	hours := func(from, to time.Time) float64 {
		total := time.Duration(0)
		for _, s := range nightSpans(from, to, loc) {
			total += s[1].Sub(s[0])
		}
		return total.Hours()
	}
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, loc) }
	if got := hours(day(2026, 10, 14), day(2026, 10, 15)); got != 7 {
		t.Fatalf("normal day night hours = %v, want 7", got)
	}
	// Spring-forward day: 00:00-06:00 loses an hour, then the 23:00 night starts.
	if got := hours(day(2026, 3, 8), day(2026, 3, 9)); got != 6 {
		t.Fatalf("spring-forward day night hours = %v, want 6", got)
	}
	// Fall-back day: 00:00-06:00 gains an hour.
	if got := hours(day(2026, 11, 1), day(2026, 11, 2)); got != 8 {
		t.Fatalf("fall-back day night hours = %v, want 8", got)
	}
	from := time.Date(2026, 10, 14, 22, 30, 0, 0, loc)
	if f := nightFraction(from, from.Add(time.Hour), loc); f.Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("straddling fraction = %s, want 1/2", f.RatString())
	}
	from = time.Date(2026, 10, 14, 5, 45, 0, 0, loc)
	if f := nightFraction(from, from.Add(30*time.Minute), loc); f.Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("06:00 straddle fraction = %s, want 1/2", f.RatString())
	}
}

func nightFixture(t *testing.T, start time.Time, intervals int, spec splitSpec, loc *time.Location) componentInput {
	t.Helper()
	fuel := "electricity"
	items := []interval{}
	for i := 0; i < intervals; i++ {
		from := start.Add(time.Duration(i) * 15 * time.Minute)
		items = append(items, interval{Start: from, End: from.Add(15 * time.Minute), Wh: mustDecimal("0.1"), Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug"})
	}
	end := start.Add(time.Duration(intervals) * 15 * time.Minute)
	now := end.Add(time.Hour)
	segment := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: now.Add(-time.Hour)}
	schedule := selectTariffSchedule(now, []interval{{Start: start, End: end, Wh: big.NewRat(1, 1), Fuel: &fuel}}, []marginalTariff{segment})
	if schedule == nil {
		t.Fatal("tariff not selected")
	}
	buckets := makeBuckets(period{Start: start, End: end}, "hour", items)
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	periodRows := sumDeviceAllocations(wh, nil)
	priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
	specs := map[string]splitSpec{"dyson": spec}
	applyMeasuredSplit(buckets, rows, periodRows, items, items, specs, schedule, nil)
	cost := money{Amount: "1.000000", Currency: "USD"}
	return componentInput{Loc: loc, Buckets: buckets, Items: items, Priced: items, Rows: rows, DeviceRows: periodRows, Specs: specs, Tariff: schedule, Metered: map[string]bool{"dyson": true}, Cost: cost}
}

func TestNightComponentStandbyIntervalsByLocalHour(t *testing.T) {
	loc := nyLoc(t)
	// 20:00-08:00 local; 7 of 12 hours are night, each 15 min interval wastes 0.1 Wh at 0.10 USD/kWh.
	in := nightFixture(t, time.Date(2026, 10, 14, 20, 0, 0, 0, loc), 48, splitSpec{StandbyWatts: 0.5}, loc)
	got := nightComponent(in).Devices["dyson"].Amount
	if got == nil || got.Cmp(mustDecimal("0.00028")) != 0 {
		t.Fatalf("night waste = %v, want 0.00028", got)
	}
	eff := assembleMeasuredEfficiency(in)
	d := eff.Devices[0]
	if d.Night == nil || d.Night.Amount != "0.000280" || d.Basis.Night != "measured" {
		t.Fatalf("device night = %+v basis %+v", d.Night, d.Basis)
	}
	if eff.NightTotalOpportunity == nil || *eff.NightTotalOpportunity != *d.Night {
		t.Fatalf("home night %+v != device night %+v", eff.NightTotalOpportunity, d.Night)
	}
	if eff.WasteOpportunity == nil || eff.WasteOpportunity.Amount != "0.000480" {
		t.Fatalf("waste opportunity = %+v, want 0.000480", eff.WasteOpportunity)
	}
}

func TestNightComponentProratesStraddlingInterval(t *testing.T) {
	loc := nyLoc(t)
	// One 15-minute interval from 22:50 to 23:05 local: one third is night.
	in := nightFixture(t, time.Date(2026, 10, 14, 22, 50, 0, 0, loc), 1, splitSpec{StandbyWatts: 0.5}, loc)
	got := nightComponent(in).Devices["dyson"].Amount
	want := new(big.Rat).Quo(mustDecimal("0.00001"), big.NewRat(3, 1))
	if got == nil || got.Cmp(want) != 0 {
		t.Fatalf("night waste = %v, want %s", got, want.FloatString(9))
	}
}

func TestNightComponentFloorDeviceSplitsByCoveredTime(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 20, 0, 0, 0, loc), 48, splitSpec{FloorUSDPerDay: 0.000001}, loc)
	eff := assembleMeasuredEfficiency(in)
	d := eff.Devices[0]
	if d.Night == nil || eff.WasteOpportunity == nil {
		t.Fatalf("floor device has no night: %+v", eff)
	}
	if mustDecimal(d.Night.Amount).Cmp(mustDecimal(eff.WasteOpportunity.Amount)) > 0 {
		t.Fatalf("night %s exceeds waste %s", d.Night.Amount, eff.WasteOpportunity.Amount)
	}
}

func TestMeasuredEfficiencyDevicesAndHomeSum(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 20, 0, 0, 0, loc), 48, splitSpec{StandbyWatts: 0.5}, loc)
	gas := allocation{Fuel: "gas", Availability: "partial", EvidenceBasis: "estimated"}
	est := allocation{Availability: "partial", EvidenceBasis: "estimated", WasteCost: money{Amount: "0.500000", Currency: "USD"}}
	in.DeviceRows = append(in.DeviceRows, deviceAllocation{ApplianceID: "furnace", Allocation: gas}, deviceAllocation{ApplianceID: "tv", Allocation: est})
	// A metered device with no interval data has no night entry: null, insufficient_data.
	in.DeviceRows = append(in.DeviceRows, deviceAllocation{ApplianceID: "lamp", Allocation: allocation{}})
	in.Metered["lamp"] = true
	eff := assembleMeasuredEfficiency(in)
	byID := map[string]DeviceEfficiency{}
	sum := new(big.Rat)
	for _, d := range eff.Devices {
		byID[d.ApplianceID] = d
		if d.Night != nil {
			sum.Add(sum, mustDecimal(d.Night.Amount))
		}
	}
	if d := byID["furnace"]; d.Night != nil || d.Basis.Night != "fuel_not_applicable" || d.Basis.Habits != "fuel_not_applicable" {
		t.Fatalf("gas device = %+v", d)
	}
	if d := byID["tv"]; d.Night != nil || d.Away != nil || d.Basis.Night != "unmetered" || d.Basis.Peak != "unmetered" {
		t.Fatalf("unmetered device = %+v", d)
	}
	if d := byID["lamp"]; d.Night != nil || d.Basis.Night != "insufficient_data" {
		t.Fatalf("metered device without night entry = %+v", d)
	}
	if eff.NightTotalOpportunity == nil || mustDecimal(eff.NightTotalOpportunity.Amount).Cmp(sum) != 0 {
		t.Fatalf("home night %+v != device sum %s", eff.NightTotalOpportunity, sum.FloatString(6))
	}
	if eff.WasteBasis != "estimated" || eff.WasteOpportunity.Amount != "0.500480" {
		t.Fatalf("waste = %+v basis %s", eff.WasteOpportunity, eff.WasteBasis)
	}
	if eff.AwayOpportunity != nil || eff.AwayBasis != "not_configured" || eff.AwaySource != "none" ||
		eff.PeakRatesOpportunity != nil || eff.PeakRatesBasis != "flat_tariff" ||
		eff.HabitsOpportunity != nil || eff.HabitsBasis != "insufficient_data" {
		t.Fatalf("stub components = %+v", eff)
	}
	if eff.IdentifiedOpportunity.Amount != eff.NightTotalOpportunity.Amount {
		t.Fatalf("identified %s != night %s", eff.IdentifiedOpportunity.Amount, eff.NightTotalOpportunity.Amount)
	}
	if eff.ChartSegmentWidth != (chartSegmentWidthPercent{"100.00", "0.00", "0.00", "0.00"}) {
		t.Fatalf("chart widths = %+v", eff.ChartSegmentWidth)
	}
}

func TestMeasuredEfficiencyWithoutMeteredDevices(t *testing.T) {
	in := componentInput{Loc: time.UTC, Cost: money{Amount: "2.000000", Currency: "USD"}, Metered: map[string]bool{}}
	in.DeviceRows = []deviceAllocation{{ApplianceID: "furnace", Allocation: allocation{Fuel: "gas"}}}
	eff := assembleMeasuredEfficiency(in)
	if eff.NightBasis != "fuel_not_applicable" || eff.WasteBasis != "fuel_not_applicable" || eff.WasteOpportunity != nil || eff.OptimalBase.Amount != "2.000000" {
		t.Fatalf("gas-only = %+v", eff)
	}
	in.DeviceRows = []deviceAllocation{{ApplianceID: "tv", Allocation: allocation{}}}
	if eff = assembleMeasuredEfficiency(in); eff.NightBasis != "unmetered" || eff.WasteBasis != "unmetered" {
		t.Fatalf("unmetered-only = %+v", eff)
	}
}

func TestMeasuredEfficiencyJSONMatchesContractShape(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 20, 0, 0, 0, loc), 48, splitSpec{StandbyWatts: 0.5}, loc)
	raw, err := os.ReadFile("../../../energylens-common-contracts/api/household-energy.schema.json")
	if err != nil {
		t.Skip("contracts repo not present")
	}
	var schema struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{
		"efficiencyMonthToDate":    measuredEfficiencyMonthToDate(in),
		"efficiencyTrailing30Days": measuredEfficiencyTrailing30Days(in),
	}
	for name, v := range cases {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]json.RawMessage{}
		if err = json.Unmarshal(out, &fields); err != nil {
			t.Fatal(err)
		}
		def := schema.Defs[name]
		for key := range fields {
			if _, ok := def.Properties[key]; !ok {
				t.Errorf("%s: unexpected property %s", name, key)
			}
		}
		for _, key := range def.Required {
			if _, ok := fields[key]; !ok {
				t.Errorf("%s: missing required %s", name, key)
			}
		}
		for _, key := range []string{"night_basis", "away_basis", "peak_rates_basis", "habits_basis", "waste_basis", "night_window", "away_source"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("%s: measured field %s missing", name, key)
			}
		}
		var devices []map[string]json.RawMessage
		if err = json.Unmarshal(fields["devices"], &devices); err != nil || len(devices) != 1 {
			t.Fatalf("%s devices = %s (%v)", name, fields["devices"], err)
		}
		for _, key := range schema.Defs["efficiencyDevice"].Required {
			if _, ok := devices[0][key]; !ok {
				t.Errorf("%s: device missing %s", name, key)
			}
		}
	}
}
