package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

// occupancyFixture meters a 0.4 W standby device and a load whose draw in W
// comes from occupied(local time), in 15-minute intervals from start.
func occupancyFixture(t *testing.T, start time.Time, days int, loc *time.Location, occupied func(time.Time) string) componentInput {
	t.Helper()
	fuel := "electricity"
	end := start.AddDate(0, 0, days)
	items := []interval{}
	for from := start; from.Before(end); from = from.Add(15 * time.Minute) {
		to := from.Add(15 * time.Minute)
		items = append(items,
			interval{Start: from, End: to, Wh: mustDecimal("0.1"), Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug", SensorID: "s-dyson", TopologyComplete: true, TopologyTerminal: true},
			interval{Start: from, End: to, Wh: new(big.Rat).Quo(mustDecimal(occupied(from.In(loc))), big.NewRat(4, 1)), Fuel: &fuel, ApplianceID: "tv", SensorKind: "plug", SensorID: "s-tv", TopologyComplete: true, TopologyTerminal: true})
	}
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
	specs := map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}
	applyMeasuredSplit(buckets, rows, periodRows, items, items, specs, schedule, nil)
	cost := money{Amount: "1.000000", Currency: "USD"}
	return componentInput{Loc: loc, Buckets: buckets, Items: items, Priced: items, Rows: rows, DeviceRows: periodRows, Specs: specs, Tariff: schedule, Metered: map[string]bool{"dyson": true, "tv": true}, Cost: cost}
}

func inWindow(t time.Time, day time.Weekday, from, to int) bool {
	return t.Weekday() == day && t.Hour() >= from && t.Hour() < to
}

// awakeWeek: home 06:00-23:00 at 100 W; Wednesday 09:00-17:00 away, Thursday
// 09:00-17:00 quiet at 2 W, Friday one away hour 12:00-13:00.
func awakeWeek(l time.Time) string {
	switch {
	case l.Hour() < 6 || l.Hour() >= 23:
		return "0"
	case inWindow(l, time.Wednesday, 9, 17), inWindow(l, time.Friday, 12, 13):
		return "0"
	case inWindow(l, time.Thursday, 9, 17):
		return "2"
	}
	return "100"
}

func TestInferredAwayFindsAwayDayNotQuietDay(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	spans := inferAwaySpans(in)
	want := [2]time.Time{time.Date(2026, 10, 14, 9, 0, 0, 0, loc), time.Date(2026, 10, 14, 17, 0, 0, 0, loc)}
	if len(spans) != 1 || !spans[0][0].Equal(want[0]) || !spans[0][1].Equal(want[1]) {
		t.Fatalf("inferred spans = %v, want %v", spans, want)
	}
	res := awayComponent(in)
	got := res.Devices["dyson"]
	// 32 standby intervals in 8 away hours, 0.1 Wh each at 0.10 USD/kWh.
	if wantCost := new(big.Rat).Mul(big.NewRat(32, 1), mustDecimal("0.00001")); got.Amount == nil || got.Amount.Cmp(wantCost) != 0 || got.Basis != "estimated" || res.Source != "baseline_inferred" {
		t.Fatalf("inferred away = %+v %+v", res, got)
	}
	eff := assembleMeasuredEfficiency(in)
	if eff.AwaySource != "baseline_inferred" || eff.AwayBasis != "estimated" || eff.AwayOpportunity == nil || eff.AwayOpportunity.Amount != "0.000320" {
		t.Fatalf("home away = %+v", eff)
	}
}

func TestInferredAwayRunMayEndAtNightButExcludesIt(t *testing.T) {
	loc := nyLoc(t)
	// Saturday leaves at 21:00 and returns Sunday 08:00: away is 21:00-23:00 and 06:00-08:00.
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, func(l time.Time) string {
		if inWindow(l, time.Saturday, 21, 24) || inWindow(l, time.Sunday, 0, 8) {
			return "0"
		}
		return awakeWeek(l)
	})
	spans := inferAwaySpans(in)
	if h := spanHours(spans); len(spans) != 3 || h != 12 {
		t.Fatalf("spans = %v (%v h)", spans, h)
	}
	for _, s := range spans {
		if len(nightSpans(s[0], s[1], loc)) != 0 {
			t.Fatalf("inferred %v overlaps night", s)
		}
	}
	night := nightComponent(in).Devices["dyson"].Amount
	away := awayComponent(in).Devices["dyson"].Amount
	if total := new(big.Rat).Mul(big.NewRat(7*96, 1), mustDecimal("0.00001")); new(big.Rat).Add(night, away).Cmp(total) > 0 {
		t.Fatalf("night %v + away %v exceeds total", night, away)
	}
}

func TestInferredAwayOrder(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	user := awaySchedule{{Days: awayAllDays, StartLocal: "09:00", EndLocal: "10:00"}}
	if res := awayComponentFor(in, user); res.Source != "user_schedule" || res.Devices["dyson"].Basis != "measured" {
		t.Fatalf("user schedule did not win: %+v", res)
	}
	flat := nightFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7*96, splitSpec{StandbyWatts: 0.5}, loc)
	if spans := inferAwaySpans(flat); spans != nil {
		t.Fatalf("flat draw inferred %v", spans)
	}
	if res := awayComponent(flat); res.Source != "default_schedule" {
		t.Fatalf("flat draw source = %s, want default_schedule", res.Source)
	}
	short := occupancyFixture(t, time.Date(2026, 10, 14, 6, 0, 0, 0, loc), 1, loc, awakeWeek)
	short.Buckets = short.Buckets[:20]
	if res := awayComponent(short); res.Source != "default_schedule" {
		t.Fatalf("20 measured hours source = %s, want default_schedule", res.Source)
	}
}

func TestInferredAwayOnDSTFallBackDay(t *testing.T) {
	loc := nyLoc(t)
	// Sunday 2026-11-01 has 25 hours; away 10:00-14:00 local.
	in := occupancyFixture(t, time.Date(2026, 10, 26, 0, 0, 0, 0, loc), 7, loc, func(l time.Time) string {
		if inWindow(l, time.Sunday, 10, 14) {
			return "0"
		}
		return awakeWeek(l)
	})
	day := civilDay(time.Date(2026, 11, 1, 12, 0, 0, 0, loc), loc)
	n := 0
	for _, h := range homeHours(in.Items, in.Buckets[0].Start, in.Buckets[len(in.Buckets)-1].End, loc) {
		if h.Day == day {
			n++
		}
	}
	if n != 25 {
		t.Fatalf("fall-back day has %d hours, want 25", n)
	}
	var sunday [][2]time.Time
	for _, s := range inferAwaySpans(in) {
		if civilDay(s[0], loc) == day {
			sunday = append(sunday, s)
		}
	}
	if len(sunday) != 1 || !sunday[0][0].Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, loc)) || spanHours(sunday) != 4 {
		t.Fatalf("DST day spans = %v", sunday)
	}
}
