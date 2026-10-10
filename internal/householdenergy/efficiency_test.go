package householdenergy

import (
	"fmt"
	"testing"
	"time"
)

func TestSyntheticEfficiencyMonthToDateUsesAllWasteAndSeparateDemoOpportunities(t *testing.T) {
	s := demoSnapshotForTest()
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, loc)
	_, total, _, _ := projectDemoSnapshot(s, monthToDatePeriod(now, loc), "day", now)
	got := syntheticEfficiencyMonthToDate(total)
	if got == nil {
		t.Fatal("valid synthetic MTD total has no efficiency projection")
	}
	if got.MonthToDateCost.Amount != "6.840000" || got.WasteOpportunity.Amount != "0.840000" ||
		got.OptimalBase.Amount != "6.000000" || got.IdentifiedOpportunity.Amount != "16.140000" ||
		got.AwayOpportunity.Amount != "4.200000" || got.NightTotalOpportunity.Amount != "0.630000" ||
		got.AwayTotalOpportunity.Amount != "4.410000" || got.PeakRatesOpportunity.Amount != "4.700000" ||
		got.HabitsOpportunity.Amount != "6.400000" || got.EvidenceBasis != "synthetic" ||
		got.WasteTimingBasis != "unknown_75_25_demo_fallback" {
		t.Fatalf("unexpected MTD demo efficiency projection: %+v", got)
	}
	if sum := moneyMicrosUnits(got.NightTotalOpportunity) + moneyMicrosUnits(got.AwayTotalOpportunity) +
		moneyMicrosUnits(got.PeakRatesOpportunity) + moneyMicrosUnits(got.HabitsOpportunity); sum != moneyMicrosUnits(got.IdentifiedOpportunity) {
		t.Fatalf("four final rows sum to %d, identified is %d", sum, moneyMicrosUnits(got.IdentifiedOpportunity))
	}
	assertChartWidthsSumTo100(t, got.ChartSegmentWidth)
}

func TestSyntheticEfficiencyMonthToDateAssignsIndivisibleWasteToAway(t *testing.T) {
	got := syntheticEfficiencyMonthToDate(allocation{Cost: moneyMicros(1), WasteCost: moneyMicros(1), EvidenceBasis: "synthetic"})
	if got == nil || got.NightTotalOpportunity.Amount != "0.000000" || got.AwayTotalOpportunity.Amount != "4.200001" ||
		got.IdentifiedOpportunity.Amount != "15.300001" || got.OptimalBase.Amount != "0.000000" {
		t.Fatalf("sub-cent Waste was dropped or formula changed: %+v", got)
	}
	assertChartWidthsSumTo100(t, got.ChartSegmentWidth)
}

func TestOpportunityChartWidthsUseLargestRemainders(t *testing.T) {
	got := opportunityChartWidths([4]int64{1, 1, 1, 0})
	if got != (chartSegmentWidthPercent{"33.34", "33.33", "33.33", "0.00"}) {
		t.Fatalf("thirds did not reconcile: %+v", got)
	}
	assertChartWidthsSumTo100(t, got)
	got = opportunityChartWidths([4]int64{0, 0, 0, 1})
	if got != (chartSegmentWidthPercent{"0.00", "0.00", "0.00", "100.00"}) {
		t.Fatalf("single opportunity got wrong chart widths: %+v", got)
	}
}

func assertChartWidthsSumTo100(t *testing.T, widths chartSegmentWidthPercent) {
	t.Helper()
	var total int
	for _, width := range []string{widths.Night, widths.Away, widths.PeakRates, widths.Habits} {
		var whole, fraction int
		if _, err := fmt.Sscanf(width, "%d.%02d", &whole, &fraction); err != nil {
			t.Fatalf("invalid percentage width %q: %v", width, err)
		}
		total += whole*100 + fraction
	}
	if total != 10_000 {
		t.Fatalf("chart width total = %d hundredths, want 10000: %+v", total, widths)
	}
}

func TestSyntheticEfficiencyMonthToDateRejectsUnqualifiedTotals(t *testing.T) {
	for _, total := range []allocation{
		{Cost: moneyMicros(1_000_000), WasteCost: moneyMicros(100_000), EvidenceBasis: "measured"},
		{Cost: moneyMicros(1_000_000), EvidenceBasis: "synthetic"},
		{Cost: moneyMicros(1_000_000), WasteCost: moneyMicros(1_000_001), EvidenceBasis: "synthetic"},
	} {
		if got := syntheticEfficiencyMonthToDate(total); got != nil {
			t.Fatalf("unqualified total produced demo opportunity: %+v", got)
		}
	}
}
