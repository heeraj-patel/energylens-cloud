package estimation

import "testing"

func TestWeeklyUsageProjectionPreservesZeroAndCivilDays(t *testing.T) {
	for _, tc := range []struct{ weekly, start, end, want string }{
		{"7", "2026-10-01", "2026-11-01", "31.000000"},
		{"14", "2026-03-01", "2026-04-01", "62.000000"},
		{"0", "2026-10-01", "2026-11-01", "0.000000"},
		{"1", "2024-02-01", "2024-03-01", "4.142857"},
	} {
		got, err := ProjectWeeklyUsage(tc.weekly, tc.start, tc.end, "America/Chicago")
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
	if _, err := ProjectWeeklyUsage("-1", "2026-10-01", "2026-11-01", "America/Chicago"); err == nil {
		t.Fatal("negative weekly usage accepted")
	}
}

func TestProjectedCyclesCalculateUsefulCost(t *testing.T) {
	projected, err := ProjectWeeklyUsage("7", "2026-10-01", "2026-11-01", "America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Calculate(Input{Fact: Fact{Kind: "cycle_energy", Unit: "kWh/cycle", Value: "1.5", Reviewed: true, Source: "reviewed_exact"}, Identity: "confirmed_exact", ProjectedCycles: &projected, RatePerKWh: "0.20", RateKind: "user_tariff", StartDate: "2026-10-01", EndDate: "2026-11-01", Timezone: "America/Chicago"})
	if err != nil || result.Kind != "estimated_useful_cost" || result.EnergyKWh != "46.500000" || result.Cost != "9.300000" {
		t.Fatalf("result: %+v %v", result, err)
	}
}

func TestWeeklyUsageCalculationKeepsExactCalendarFraction(t *testing.T) {
	weekly := "1"
	result, err := Calculate(Input{Fact: Fact{Kind: "cycle_energy", Unit: "kWh/cycle", Value: "0.169", Reviewed: true, Source: "reviewed_exact"}, Identity: "confirmed_exact", WeeklyUsage: &weekly, RatePerKWh: "1", RateKind: "user_tariff", StartDate: "2026-10-01", EndDate: "2026-11-01", Timezone: "America/Chicago"})
	if err != nil || result.EnergyKWh != "0.748429" || result.Cost != "0.748429" {
		t.Fatalf("exact weekly fraction: %+v %v", result, err)
	}
}
