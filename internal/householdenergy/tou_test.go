package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

func mustPeriod(t *testing.T, adder string, days []string, start, end string, months ...int16) touPeriod {
	t.Helper()
	p, err := parsePeriod(adder, days, start, end, months)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var everyDay = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func touSchedule(t *testing.T, periods ...touPeriod) marginalTariff {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	segment := marginalTariff{Rate: big.NewRat(30, 100), Currency: "USD", From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Periods: periods, Loc: loc}
	return marginalTariff{Currency: "USD", Segments: []marginalTariff{segment}}
}

// price prices uniform energy (1 kWh per hour) between two home-local wall times.
func price(t *testing.T, schedule marginalTariff, from, to time.Time, summarized bool) string {
	t.Helper()
	fuel := "electricity"
	wh := new(big.Rat).SetFrac64(to.Sub(from).Milliseconds(), 3600)
	item := interval{Start: from, End: to, Wh: wh, Fuel: &fuel, Summarized: summarized}
	cost := priceIntervalRange(item, from, to, schedule)
	if cost == nil {
		return "unpriced"
	}
	return cost.FloatString(6)
}

func TestTOUPriceChangesAt1600(t *testing.T) {
	schedule := touSchedule(t, mustPeriod(t, "0.15", everyDay, "16:00:00", "21:00:00", 6, 7, 8, 9))
	loc := schedule.Segments[0].Loc
	from := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	if got := price(t, schedule, from, from.Add(2*time.Hour), false); got != "0.750000" {
		t.Fatalf("15:00-17:00 = %s, want 0.30 + 0.45", got)
	}
	if got := price(t, schedule, from.Add(30*time.Minute), from.Add(90*time.Minute), false); got != "0.375000" {
		t.Fatalf("15:30-16:30 = %s", got)
	}
	if got := price(t, schedule, from, from.Add(time.Hour), true); got != "0.300000" {
		t.Fatalf("retained 15:00 hour = %s", got)
	}
	if got := price(t, schedule, from.Add(30*time.Minute), from.Add(90*time.Minute), true); got != "unpriced" {
		t.Fatalf("retained hour across 16:00 = %s", got)
	}
}

func TestTOUWrappingRowStaysOnItsListedDay(t *testing.T) {
	schedule := touSchedule(t, mustPeriod(t, "0.10", []string{"fri"}, "21:00:00", "07:00:00", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))
	loc := schedule.Segments[0].Loc
	friday := time.Date(2026, 10, 23, 21, 0, 0, 0, loc)
	// Friday 21-24 has the adder; Saturday 00-09 does not.
	if got := price(t, schedule, friday, friday.Add(12*time.Hour), false); got != "3.900000" {
		t.Fatalf("Friday 21:00 to Saturday 09:00 = %s", got)
	}
	saturday := time.Date(2026, 10, 24, 6, 0, 0, 0, loc)
	if got := price(t, schedule, saturday, saturday.Add(time.Hour), false); got != "0.300000" {
		t.Fatalf("Saturday 06:00 = %s", got)
	}
	early := time.Date(2026, 10, 23, 6, 0, 0, 0, loc)
	if got := price(t, schedule, early, early.Add(time.Hour), false); got != "0.400000" {
		t.Fatalf("Friday 06:00 = %s", got)
	}
}

func TestTOUMonthEdge(t *testing.T) {
	schedule := touSchedule(t, mustPeriod(t, "0.15", everyDay, "16:00:00", "21:00:00", 6, 7, 8, 9))
	loc := schedule.Segments[0].Loc
	from := time.Date(2026, 9, 30, 20, 0, 0, 0, loc)
	to := time.Date(2026, 10, 1, 17, 0, 0, 0, loc)
	// Sep 30 20-21 is peak; Oct 1 16-17 is not.
	if got := price(t, schedule, from, to, false); got != "6.450000" {
		t.Fatalf("Sep 30 20:00 to Oct 1 17:00 = %s", got)
	}
}

func TestTOUDSTDaysUseLocalWallClock(t *testing.T) {
	schedule := touSchedule(t, mustPeriod(t, "0.10", everyDay, "16:00:00", "21:00:00", 11), mustPeriod(t, "0.10", everyDay, "01:00:00", "04:00:00", 3))
	loc := schedule.Segments[0].Loc
	fallBack := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	if day := time.Date(2026, 11, 2, 0, 0, 0, 0, loc).Sub(fallBack); day != 25*time.Hour {
		t.Fatalf("fall-back day is %s", day)
	}
	if got := price(t, schedule, fallBack, time.Date(2026, 11, 2, 0, 0, 0, 0, loc), false); got != "8.000000" {
		t.Fatalf("25-hour day = %s, want 5 peak hours", got)
	}
	peak := schedule.Segments[0].pricedPieces(fallBack, time.Date(2026, 11, 2, 0, 0, 0, 0, loc))
	if len(peak) != 3 || !peak[1].from.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("fall-back pieces %+v", peak)
	}
	springForward := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	// 01:00-04:00 local is two real hours on a 23-hour day.
	if got := price(t, schedule, springForward, time.Date(2026, 3, 9, 0, 0, 0, 0, loc), false); got != "7.100000" {
		t.Fatalf("23-hour day = %s", got)
	}
}

func TestTOUFlatPlanUnchanged(t *testing.T) {
	flat := touSchedule(t)
	flat.Segments[0].Loc = nil
	from := time.Date(2026, 6, 10, 15, 0, 0, 0, time.UTC)
	if got := price(t, flat, from, from.Add(48*time.Hour), false); got != "14.400000" {
		t.Fatalf("flat plan = %s", got)
	}
	if pieces := flat.Segments[0].pricedPieces(from, from.Add(48*time.Hour)); len(pieces) != 1 {
		t.Fatalf("flat plan split into %d pieces", len(pieces))
	}
}

func TestTOUOverlapDetected(t *testing.T) {
	peak := mustPeriod(t, "0.15", everyDay, "16:00:00", "21:00:00", 6)
	wrap := mustPeriod(t, "0.03", everyDay, "21:00:00", "16:00:00", 6)
	if periodsOverlap([]touPeriod{peak, wrap}) {
		t.Fatal("adjacent peak and wrapping off-peak reported as overlapping")
	}
	if !periodsOverlap([]touPeriod{peak, mustPeriod(t, "0.02", []string{"mon"}, "20:00:00", "02:00:00", 6)}) {
		t.Fatal("overlap at 20:00 not detected")
	}
	if periodsOverlap([]touPeriod{peak, mustPeriod(t, "0.02", everyDay, "16:00:00", "21:00:00", 7)}) {
		t.Fatal("different months reported as overlapping")
	}
}
