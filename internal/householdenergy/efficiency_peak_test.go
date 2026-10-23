package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

var allMonths = []int16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
var allDays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func peakRow(t *testing.T, adder string, days []string, start, end string) touPeriod {
	t.Helper()
	p, err := parsePeriod(adder, days, start, end, allMonths)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func peakTariff(loc *time.Location, periods ...touPeriod) *marginalTariff {
	seg := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Periods: periods}
	if len(periods) > 0 {
		seg.Loc = loc
	}
	return &marginalTariff{Rate: seg.Rate, Currency: "USD", Segments: []marginalTariff{seg}}
}

// peakInput has one device per category, each using wh over [from,to).
func peakInput(loc *time.Location, tariff *marginalTariff, from, to time.Time, wh string, categories ...string) componentInput {
	fuel := "electricity"
	in := componentInput{Loc: loc, Tariff: tariff, Specs: map[string]splitSpec{}, Metered: map[string]bool{}, Cost: money{Amount: "1.000000", Currency: "USD"}}
	for _, c := range categories {
		in.Specs[c] = splitSpec{Category: c}
		in.Metered[c] = true
		in.Priced = append(in.Priced, interval{Start: from, End: to, Wh: mustDecimal(wh), Fuel: &fuel, ApplianceID: c, SensorKind: "plug"})
		in.DeviceRows = append(in.DeviceRows, deviceAllocation{ApplianceID: c, Allocation: allocation{Availability: "complete", EvidenceBasis: "measured"}})
	}
	return in
}

func standardRows(t *testing.T) []touPeriod {
	return []touPeriod{peakRow(t, "0.20", allDays, "16:00:00", "21:00:00"), peakRow(t, "0.02", allDays, "22:00:00", "06:00:00")}
}

func wantAmount(t *testing.T, res componentResult, id, want string) {
	t.Helper()
	got := res.Devices[id].Amount
	if got == nil || got.Cmp(mustDecimal(want)) != 0 {
		t.Fatalf("%s peak = %v, want %s", id, got, want)
	}
}

func TestPeakComponentFlatTariff(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 14, 17, 0, 0, 0, loc)
	res := peakComponent(peakInput(loc, peakTariff(loc), from, from.Add(time.Hour), "1000", "dishwasher"))
	if res.Basis != "flat_tariff" || len(res.Devices) != 0 {
		t.Fatalf("flat result = %+v", res)
	}
}

func TestPeakComponentTOUSavesAdderDifference(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 14, 17, 0, 0, 0, loc)
	res := peakComponent(peakInput(loc, peakTariff(loc, standardRows(t)...), from, from.Add(time.Hour), "1000", "dishwasher"))
	wantAmount(t, res, "dishwasher", "0.2")
	if res.Devices["dishwasher"].Basis != "measured" {
		t.Fatalf("basis = %q", res.Devices["dishwasher"].Basis)
	}
	// Off-peak use at the day's lowest adder saves nothing.
	from = time.Date(2026, 10, 14, 12, 0, 0, 0, loc)
	res = peakComponent(peakInput(loc, peakTariff(loc, standardRows(t)...), from, from.Add(time.Hour), "1000", "dishwasher"))
	wantAmount(t, res, "dishwasher", "0")
}

func TestPeakComponentWrappingRowIsLocalDayOnly(t *testing.T) {
	loc := nyLoc(t)
	// Friday 22:00 to Saturday 06:00 only.
	tariff := peakTariff(loc, peakRow(t, "0.30", []string{"fri"}, "22:00:00", "06:00:00"))
	run := func(from time.Time) componentResult {
		return peakComponent(peakInput(loc, tariff, from, from.Add(time.Hour), "1000", "dryer"))
	}
	// 2026-10-16 is a Friday: its 03:00 and 23:00 are inside the row; the day's lowest adder is 0.
	wantAmount(t, run(time.Date(2026, 10, 16, 23, 0, 0, 0, loc)), "dryer", "0.3")
	wantAmount(t, run(time.Date(2026, 10, 16, 3, 0, 0, 0, loc)), "dryer", "0.3")
	// Saturday 03:00 is not priced by Friday's row.
	wantAmount(t, run(time.Date(2026, 10, 17, 3, 0, 0, 0, loc)), "dryer", "0")
}

func TestPeakComponentDSTDayKeepsLocalHours(t *testing.T) {
	loc := nyLoc(t)
	// 2026-03-08 has 23 real hours; each carries 1 kWh.
	from := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	to := time.Date(2026, 3, 9, 0, 0, 0, 0, loc)
	if to.Sub(from) != 23*time.Hour {
		t.Fatalf("test day length = %v", to.Sub(from))
	}
	res := peakComponent(peakInput(loc, peakTariff(loc, standardRows(t)...), from, to, "23000", "ev_charger"))
	// 5 peak hours x 0.20 + 7 wrap hours (00-02, 03-06, 22-24) x 0.02.
	wantAmount(t, res, "ev_charger", "1.14")
}

func TestPeakComponentOnlyShiftableElectricDevices(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 14, 17, 0, 0, 0, loc)
	cats := []string{"dishwasher", "washer", "clothes_washer", "dryer", "ev_charger", "pool_pump", "water_heater", "refrigerator", "tv"}
	in := peakInput(loc, peakTariff(loc, standardRows(t)...), from, from.Add(time.Hour), "1000", cats...)
	res := peakComponent(in)
	for _, c := range cats[:7] {
		wantAmount(t, res, c, "0.2")
	}
	for _, c := range cats[7:] {
		if _, ok := res.Devices[c]; ok {
			t.Fatalf("%s should have no peak entry", c)
		}
	}
	gas := peakInput(loc, in.Tariff, from, from.Add(time.Hour), "1000", "water_heater")
	gas.DeviceRows[0].Allocation.Fuel = "natural_gas"
	if _, ok := peakComponent(gas).Devices["water_heater"]; ok {
		t.Fatal("gas water heater should be excluded")
	}
	unmetered := peakInput(loc, in.Tariff, from, from.Add(time.Hour), "1000", "dryer")
	unmetered.Metered["dryer"] = false
	if _, ok := peakComponent(unmetered).Devices["dryer"]; ok {
		t.Fatal("unmetered dryer should be excluded")
	}
}

func TestPeakOpportunityEqualsAdderDifferenceTimesShiftedKWh(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 14, 17, 0, 0, 0, loc)
	in := peakInput(loc, peakTariff(loc, standardRows(t)...), from, from.Add(time.Hour), "2500", "dishwasher", "tv")
	eff := assembleMeasuredEfficiency(in)
	if eff == nil || eff.PeakRatesOpportunity == nil {
		t.Fatalf("no peak opportunity: %+v", eff)
	}
	// 2.5 kWh x (0.20 - 0) adder difference.
	if eff.PeakRatesOpportunity.Amount != "0.500000" || eff.PeakRatesBasis != "measured" {
		t.Fatalf("peak = %+v basis %q", eff.PeakRatesOpportunity, eff.PeakRatesBasis)
	}
	for _, d := range eff.Devices {
		if d.ApplianceID == "dishwasher" && (d.Peak == nil || d.Peak.Amount != "0.500000") {
			t.Fatalf("dishwasher peak = %+v", d.Peak)
		}
		if d.ApplianceID == "tv" && d.Peak != nil {
			t.Fatalf("tv peak = %+v, want null", d.Peak)
		}
	}
	flat := assembleMeasuredEfficiency(peakInput(loc, peakTariff(loc), from, from.Add(time.Hour), "2500", "dishwasher"))
	if flat.PeakRatesOpportunity != nil || flat.PeakRatesBasis != "flat_tariff" {
		t.Fatalf("flat peak = %+v basis %q", flat.PeakRatesOpportunity, flat.PeakRatesBasis)
	}
}
