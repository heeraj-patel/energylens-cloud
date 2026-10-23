package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

var awayAllDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func spanHours(spans [][2]time.Time) float64 {
	var d time.Duration
	for _, s := range spans {
		d += s[1].Sub(s[0])
	}
	return d.Hours()
}

func TestAwaySpansDefaultIsNineToFourEveryDay(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, loc)
	spans := awaySpans(from, from.AddDate(0, 0, 7), loc, defaultAwaySchedule)
	if len(spans) != 7 || spanHours(spans) != 49 {
		t.Fatalf("default spans = %v", spans)
	}
	if !spans[0][0].Equal(time.Date(2026, 10, 12, 9, 0, 0, 0, loc)) || !spans[0][1].Equal(time.Date(2026, 10, 12, 16, 0, 0, 0, loc)) {
		t.Fatalf("first span = %v", spans[0])
	}
}

func TestAwaySpansExcludeNightWindow(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, loc)
	// Monday 20:00-08:00 keeps only 20:00-23:00 and Tuesday 06:00-08:00.
	sched := awaySchedule{{Days: []string{"mon"}, StartLocal: "20:00", EndLocal: "08:00"}}
	spans := awaySpans(from, from.AddDate(0, 0, 2), loc, sched)
	if len(spans) != 2 || spanHours(spans) != 5 {
		t.Fatalf("spans = %v", spans)
	}
	for _, s := range spans {
		for _, n := range nightSpans(from, from.AddDate(0, 0, 2), loc) {
			if s[0].Before(n[1]) && n[0].Before(s[1]) {
				t.Fatalf("away %v overlaps night %v", s, n)
			}
		}
	}
	whole := awaySchedule{{Days: awayAllDays, StartLocal: "00:00", EndLocal: "24:00"}}
	if h := spanHours(awaySpans(from, from.AddDate(0, 0, 1), loc, whole)); h != 17 {
		t.Fatalf("all-day away outside night = %v h, want 17", h)
	}
}

func TestAwaySpansMergeOverlappingBlocksAndDST(t *testing.T) {
	loc := nyLoc(t)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	sched := awaySchedule{
		{Days: []string{"sun"}, StartLocal: "09:00", EndLocal: "12:00"},
		{Days: []string{"sun"}, StartLocal: "11:00", EndLocal: "14:00"},
	}
	if h := spanHours(awaySpans(from, from.AddDate(0, 0, 1), loc, sched)); h != 5 {
		t.Fatalf("merged away = %v h, want 5", h)
	}
}

func TestAwayComponentDefaultIsEstimated(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 6, 0, 0, 0, loc), 96, splitSpec{StandbyWatts: 0.5}, loc)
	res := awayComponent(in)
	got := res.Devices["dyson"]
	want := new(big.Rat).Mul(big.NewRat(28, 1), mustDecimal("0.00001"))
	if got.Amount == nil || got.Amount.Cmp(want) != 0 || got.Basis != "estimated" || res.Basis != "estimated" || res.Source != "default_schedule" {
		t.Fatalf("default away = %+v %+v, want %s", res, got, want.FloatString(8))
	}
	eff := assembleMeasuredEfficiency(in)
	if eff.AwaySource != "default_schedule" || eff.AwayBasis != "estimated" || eff.AwayOpportunity == nil || eff.Devices[0].Basis.Away != "estimated" {
		t.Fatalf("home away = %+v", eff)
	}
}

func TestAwayUserScheduleWinsOverDefault(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 6, 0, 0, 0, loc), 96, splitSpec{StandbyWatts: 0.5}, loc)
	user := awaySchedule{{Days: awayAllDays, StartLocal: "09:00", EndLocal: "10:00"}}
	res := awayComponentFor(in, user)
	want := new(big.Rat).Mul(big.NewRat(4, 1), mustDecimal("0.00001"))
	got := res.Devices["dyson"]
	if got.Amount == nil || got.Amount.Cmp(want) != 0 || res.Source != "user_schedule" || got.Basis != "measured" {
		t.Fatalf("user away = %+v %+v, want %s", res, got, want.FloatString(8))
	}
}

func TestAwayDoesNotDoubleCountNight(t *testing.T) {
	loc := nyLoc(t)
	in := nightFixture(t, time.Date(2026, 10, 14, 6, 0, 0, 0, loc), 96, splitSpec{StandbyWatts: 0.5}, loc)
	// 22:00-08:00 overlaps night 23:00-06:00; away keeps 22:00-23:00 and 06:00-08:00 (12 intervals).
	user := awaySchedule{{Days: awayAllDays, StartLocal: "22:00", EndLocal: "08:00"}}
	away := awayComponentFor(in, user).Devices["dyson"].Amount
	want := new(big.Rat).Mul(big.NewRat(12, 1), mustDecimal("0.00001"))
	if away == nil || away.Cmp(want) != 0 {
		t.Fatalf("away = %v, want %s", away, want.FloatString(8))
	}
	// 28 night intervals plus 12 away intervals never exceed the 96 measured.
	night := nightComponent(in).Devices["dyson"].Amount
	if night == nil || new(big.Rat).Add(night, away).Cmp(new(big.Rat).Mul(big.NewRat(96, 1), mustDecimal("0.00001"))) > 0 {
		t.Fatalf("night %v + away %v exceeds total", night, away)
	}
	inNight := awaySchedule{{Days: awayAllDays, StartLocal: "23:00", EndLocal: "05:00"}}
	if got := awayComponentFor(in, inNight).Devices["dyson"].Amount; got != nil && got.Sign() != 0 {
		t.Fatalf("schedule inside night counted %v", got)
	}
}

func TestAwayComponentWithoutTariffIsInsufficient(t *testing.T) {
	if res := awayComponent(componentInput{}); res.Basis != "insufficient_data" || res.Source != "none" || len(res.Devices) != 0 {
		t.Fatalf("no tariff = %+v", res)
	}
}
