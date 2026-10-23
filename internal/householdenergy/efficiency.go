package householdenergy

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
)

// efficiencyMonthToDate is an additive demo projection. The three category
// amounts are owner-approved illustrative MTD opportunities, separate from
// the imported snapshot's avoidable Waste. None represents measured savings.
type efficiencyMonthToDate struct {
	MonthToDateCost       money                    `json:"month_to_date_cost"`
	IdentifiedOpportunity money                    `json:"identified_opportunity"`
	OptimalBase           money                    `json:"optimal_base"`
	WasteOpportunity      money                    `json:"waste_opportunity"`
	AwayOpportunity       money                    `json:"away_opportunity"`
	NightTotalOpportunity money                    `json:"night_total_opportunity"`
	AwayTotalOpportunity  money                    `json:"away_total_opportunity"`
	PeakRatesOpportunity  money                    `json:"peak_rates_opportunity"`
	HabitsOpportunity     money                    `json:"habits_opportunity"`
	ChartSegmentWidth     chartSegmentWidthPercent `json:"chart_segment_width_percent"`
	WasteTimingBasis      string                   `json:"waste_timing_basis"`
	EvidenceBasis         string                   `json:"evidence_basis"`
	measured              *MeasuredEfficiency
}

func (e efficiencyMonthToDate) MarshalJSON() ([]byte, error) {
	if e.measured != nil {
		return e.measured.marshalWithCost("month_to_date_cost")
	}
	type plain efficiencyMonthToDate
	return json.Marshal(plain(e))
}

// The rolling projection shares the same calculation, but names its cost for
// the requested window rather than implying a calendar month-to-date total.
type efficiencyTrailing30Days struct {
	PeriodCost            money                    `json:"period_cost"`
	IdentifiedOpportunity money                    `json:"identified_opportunity"`
	OptimalBase           money                    `json:"optimal_base"`
	WasteOpportunity      money                    `json:"waste_opportunity"`
	AwayOpportunity       money                    `json:"away_opportunity"`
	NightTotalOpportunity money                    `json:"night_total_opportunity"`
	AwayTotalOpportunity  money                    `json:"away_total_opportunity"`
	PeakRatesOpportunity  money                    `json:"peak_rates_opportunity"`
	HabitsOpportunity     money                    `json:"habits_opportunity"`
	ChartSegmentWidth     chartSegmentWidthPercent `json:"chart_segment_width_percent"`
	WasteTimingBasis      string                   `json:"waste_timing_basis"`
	EvidenceBasis         string                   `json:"evidence_basis"`
	measured              *MeasuredEfficiency
}

func (e efficiencyTrailing30Days) MarshalJSON() ([]byte, error) {
	if e.measured != nil {
		return e.measured.marshalWithCost("period_cost")
	}
	type plain efficiencyTrailing30Days
	return json.Marshal(plain(e))
}

func syntheticEfficiencyTrailing30Days(total allocation) *efficiencyTrailing30Days {
	projection := syntheticEfficiencyMonthToDate(total)
	if projection == nil {
		return nil
	}
	return &efficiencyTrailing30Days{
		PeriodCost:            projection.MonthToDateCost,
		IdentifiedOpportunity: projection.IdentifiedOpportunity,
		OptimalBase:           projection.OptimalBase,
		WasteOpportunity:      projection.WasteOpportunity,
		AwayOpportunity:       projection.AwayOpportunity,
		NightTotalOpportunity: projection.NightTotalOpportunity,
		AwayTotalOpportunity:  projection.AwayTotalOpportunity,
		PeakRatesOpportunity:  projection.PeakRatesOpportunity,
		HabitsOpportunity:     projection.HabitsOpportunity,
		ChartSegmentWidth:     projection.ChartSegmentWidth,
		WasteTimingBasis:      projection.WasteTimingBasis,
		EvidenceBasis:         projection.EvidenceBasis,
	}
}

type chartSegmentWidthPercent struct {
	Night     string `json:"night"`
	Away      string `json:"away"`
	PeakRates string `json:"peak_rates"`
	Habits    string `json:"habits"`
}

const (
	demoAwayOpportunityMicros      int64 = 4_200_000
	demoPeakRatesOpportunityMicros int64 = 4_700_000
	demoHabitsOpportunityMicros    int64 = 6_400_000
)

func syntheticEfficiencyMonthToDate(total allocation) *efficiencyMonthToDate {
	if total.EvidenceBasis != "synthetic" {
		return nil
	}
	cost, costOK := total.Cost.(money)
	waste, wasteOK := total.WasteCost.(money)
	if !costOK || !wasteOK || cost.Currency != "USD" || waste.Currency != "USD" {
		return nil
	}
	costMicros := moneyMicrosUnits(cost)
	wasteMicros := moneyMicrosUnits(waste)
	if wasteMicros < 0 || wasteMicros > costMicros {
		return nil
	}
	// The imported demo snapshot has no idle timing. Allocate its avoidable
	// Waste 75/25 in microcurrency units, with any indivisible remainder Away.
	nightMicros := wasteMicros * 3 / 4
	awayMicros := wasteMicros - nightMicros + demoAwayOpportunityMicros
	segments := [4]int64{nightMicros, awayMicros, demoPeakRatesOpportunityMicros, demoHabitsOpportunityMicros}
	return &efficiencyMonthToDate{
		MonthToDateCost:       cost,
		IdentifiedOpportunity: moneyMicros(sumOpportunityMicros(segments)),
		OptimalBase:           moneyMicros(costMicros - wasteMicros),
		WasteOpportunity:      waste,
		AwayOpportunity:       moneyMicros(demoAwayOpportunityMicros),
		NightTotalOpportunity: moneyMicros(nightMicros),
		AwayTotalOpportunity:  moneyMicros(awayMicros),
		PeakRatesOpportunity:  moneyMicros(demoPeakRatesOpportunityMicros),
		HabitsOpportunity:     moneyMicros(demoHabitsOpportunityMicros),
		ChartSegmentWidth:     opportunityChartWidths(segments),
		WasteTimingBasis:      "unknown_75_25_demo_fallback",
		EvidenceBasis:         "synthetic",
	}
}

func sumOpportunityMicros(segments [4]int64) int64 {
	return segments[0] + segments[1] + segments[2] + segments[3]
}

// opportunityChartWidths apportions 10,000 hundredths of a percent using
// largest remainders. The rendered segment widths therefore sum to 100.00%.
func opportunityChartWidths(segments [4]int64) chartSegmentWidthPercent {
	total := sumOpportunityMicros(segments)
	if total <= 0 {
		return chartSegmentWidthPercent{"0.00", "0.00", "0.00", "0.00"}
	}
	var widths [4]int64
	var remainders [4]*big.Int
	denominator := big.NewInt(total)
	used := int64(0)
	for i, amount := range segments {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(new(big.Int).Mul(big.NewInt(amount), big.NewInt(10_000)), denominator, remainder)
		widths[i] = quotient.Int64()
		remainders[i] = remainder
		used += widths[i]
	}
	order := []int{0, 1, 2, 3}
	sort.SliceStable(order, func(i, j int) bool {
		return remainders[order[i]].Cmp(remainders[order[j]]) > 0
	})
	for i := int64(0); i < 10_000-used; i++ {
		widths[order[i]]++
	}
	format := func(v int64) string { return fmt.Sprintf("%d.%02d", v/100, v%100) }
	return chartSegmentWidthPercent{format(widths[0]), format(widths[1]), format(widths[2]), format(widths[3])}
}
