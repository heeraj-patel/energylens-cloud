package householdenergy

import (
	"testing"
	"time"
)

func TestTOUDayPeakWindowAndSpread(t *testing.T) {
	s := touSchedule(t,
		mustPeriod(t, "0.20", everyDay, "16:00:00", "21:00:00", 7),
		mustPeriod(t, "-0.05", everyDay, "00:00:00", "06:00:00", 7))
	loc := s.Segments[0].Loc
	got := touDayOf(&s.Segments[0], time.Date(2026, 7, 10, 12, 0, 0, 0, loc))
	if got == nil || got.SavingPerKWh.FloatString(2) != "0.25" || got.PeakStart != "16:00" || got.PeakEnd != "21:00" || got.Currency != "USD" {
		t.Fatalf("%+v", got)
	}
}

func TestTOUDayWrappingPeak(t *testing.T) {
	s := touSchedule(t, mustPeriod(t, "0.10", everyDay, "22:00:00", "02:00:00", 7))
	loc := s.Segments[0].Loc
	got := touDayOf(&s.Segments[0], time.Date(2026, 7, 10, 12, 0, 0, 0, loc))
	if got == nil || got.PeakStart != "22:00" || got.PeakEnd != "02:00" || got.SavingPerKWh.FloatString(2) != "0.10" {
		t.Fatalf("%+v", got)
	}
}

func TestTOUDayNoSpread(t *testing.T) {
	s := touSchedule(t, mustPeriod(t, "0.10", everyDay, "16:00:00", "21:00:00", 7))
	loc := s.Segments[0].Loc
	if touDayOf(&s.Segments[0], time.Date(2026, 1, 10, 12, 0, 0, 0, loc)) != nil {
		t.Fatal("a day no period covers has no spread")
	}
	flat := touSchedule(t)
	if touDayOf(&flat.Segments[0], time.Date(2026, 7, 10, 12, 0, 0, 0, loc)) != nil {
		t.Fatal("flat plan has no spread")
	}
	allDay := touSchedule(t, mustPeriod(t, "0.10", everyDay, "00:00:00", "24:00:00", 7))
	if touDayOf(&allDay.Segments[0], time.Date(2026, 7, 10, 12, 0, 0, 0, loc)) != nil {
		t.Fatal("uniform adder has no spread")
	}
}
