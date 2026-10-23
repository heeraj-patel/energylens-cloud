package householdenergy

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"
	"time"
)

// componentInput is what every efficiency component reads. Rows are the
// per-bucket measured device rows; DeviceRows are the period rows.
type componentInput struct {
	Loc        *time.Location
	Buckets    []bucket
	Items      []interval
	Priced     []interval
	Rows       [][]deviceAllocation
	DeviceRows []deviceAllocation
	Specs      map[string]splitSpec
	Tariff     *marginalTariff
	Metered    map[string]bool
	Cost       money
	Away       awaySchedule
	Habits     []habitCycle
}

// deviceAmount is one device's component cost; a nil Amount is null with Basis.
type deviceAmount struct {
	Amount *big.Rat
	Basis  string
}

// componentResult holds a component's per-device amounts. Metered devices
// without an entry are null with the default Basis. Source labels the away source.
type componentResult struct {
	Devices map[string]deviceAmount
	Basis   string
	Source  string
}

type DeviceEfficiencyBasis struct {
	Night  string `json:"night"`
	Away   string `json:"away"`
	Peak   string `json:"peak"`
	Habits string `json:"habits"`
}

// DeviceEfficiency is one appliance's component costs; nil is null.
type DeviceEfficiency struct {
	ApplianceID string                `json:"appliance_id"`
	Night       *money                `json:"night"`
	Away        *money                `json:"away"`
	Peak        *money                `json:"peak"`
	Habits      *money                `json:"habits"`
	Basis       DeviceEfficiencyBasis `json:"basis"`
}

type EfficiencyNightWindow struct {
	StartLocal string `json:"start_local"`
	EndLocal   string `json:"end_local"`
	Timezone   string `json:"timezone"`
}

// MeasuredEfficiency is the evidence_basis measured efficiency object; Cost is
// emitted as month_to_date_cost or period_cost by the window wrapper.
type MeasuredEfficiency struct {
	Cost                  money                    `json:"-"`
	IdentifiedOpportunity money                    `json:"identified_opportunity"`
	OptimalBase           money                    `json:"optimal_base"`
	WasteOpportunity      *money                   `json:"waste_opportunity"`
	AwayOpportunity       *money                   `json:"away_opportunity"`
	NightTotalOpportunity *money                   `json:"night_total_opportunity"`
	AwayTotalOpportunity  *money                   `json:"away_total_opportunity"`
	PeakRatesOpportunity  *money                   `json:"peak_rates_opportunity"`
	HabitsOpportunity     *money                   `json:"habits_opportunity"`
	ChartSegmentWidth     chartSegmentWidthPercent `json:"chart_segment_width_percent"`
	WasteTimingBasis      string                   `json:"waste_timing_basis"`
	EvidenceBasis         string                   `json:"evidence_basis"`
	NightBasis            string                   `json:"night_basis"`
	AwayBasis             string                   `json:"away_basis"`
	PeakRatesBasis        string                   `json:"peak_rates_basis"`
	HabitsBasis           string                   `json:"habits_basis"`
	WasteBasis            string                   `json:"waste_basis"`
	NightWindow           EfficiencyNightWindow    `json:"night_window"`
	AwaySource            string                   `json:"away_source"`
	Devices               []DeviceEfficiency       `json:"devices"`
}

func (m MeasuredEfficiency) marshalWithCost(key string) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields[key], err = json.Marshal(m.Cost); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func measuredEfficiencyMonthToDate(in componentInput) *efficiencyMonthToDate {
	if m := assembleMeasuredEfficiency(in); m != nil {
		return &efficiencyMonthToDate{measured: m}
	}
	return nil
}

func measuredEfficiencyTrailing30Days(in componentInput) *efficiencyTrailing30Days {
	if m := assembleMeasuredEfficiency(in); m != nil {
		return &efficiencyTrailing30Days{measured: m}
	}
	return nil
}

// TrailingEfficiency is the measured trailing-30-day efficiency for a home, or
// nil when the home has no measured cost (demo homes keep the synthetic split).
func (s Service) TrailingEfficiency(ctx context.Context, owner, home string) (*MeasuredEfficiency, error) {
	c, err := s.compute(ctx, owner, home, "day", "trailing_30_days")
	if err != nil || c.trailingEfficiency == nil {
		return nil, err
	}
	return c.trailingEfficiency.measured, nil
}

func roundedRat(v *big.Rat) *big.Rat { return mustDecimal(v.FloatString(6)) }

func ratMoney(v *big.Rat, currency string) *money {
	return &money{Amount: v.FloatString(6), Currency: currency}
}

// assembleMeasuredEfficiency builds per-device components and sums them to the
// home object, so the home figure always equals the device sum.
func assembleMeasuredEfficiency(in componentInput) *MeasuredEfficiency {
	cur := in.Cost.Currency
	if cur == "" || in.Loc == nil {
		return nil
	}
	cost := mustDecimal(in.Cost.Amount)
	comps := [4]componentResult{nightComponent(in), awayComponent(in), peakComponent(in), habitsComponent(in)}
	rows := map[string]allocation{}
	ids := []string{}
	for _, r := range in.DeviceRows {
		if r.ApplianceID == "" || r.DeviceID != "" {
			continue
		}
		if _, dup := rows[r.ApplianceID]; !dup {
			rows[r.ApplianceID] = r.Allocation
			ids = append(ids, r.ApplianceID)
		}
	}
	sort.Strings(ids)
	var totals [4]*big.Rat
	var estimatedBasis [4]bool
	var wasteSum, meteredWaste *big.Rat
	var estimatedWaste, anyMetered, anyUnmetered, anyGas bool
	devices := []DeviceEfficiency{}
	for _, id := range ids {
		a := rows[id]
		var waste *big.Rat
		if w, ok := a.WasteCost.(money); ok && w.Currency == cur {
			waste = mustDecimal(w.Amount)
		}
		d := DeviceEfficiency{ApplianceID: id}
		amounts := [4]**money{&d.Night, &d.Away, &d.Peak, &d.Habits}
		bases := [4]*string{&d.Basis.Night, &d.Basis.Away, &d.Basis.Peak, &d.Basis.Habits}
		switch {
		case a.Fuel != "" && a.Fuel != "electricity":
			anyGas = true
			for k := range bases {
				*bases[k] = "fuel_not_applicable"
			}
		case !in.Metered[id]:
			anyUnmetered = true
			for k := range bases {
				*bases[k] = "unmetered"
			}
			if waste != nil {
				addRat(&wasteSum, waste)
				estimatedWaste = true
			}
		default:
			anyMetered = true
			if waste != nil {
				addRat(&wasteSum, waste)
				addRat(&meteredWaste, waste)
			}
			for k := range comps {
				got, ok := comps[k].Devices[id]
				if ok && got.Amount != nil && (k != 0 || waste != nil) {
					v := got.Amount
					if k == 0 && v.Cmp(waste) > 0 {
						v = waste
					}
					v = roundedRat(v)
					*amounts[k], *bases[k] = ratMoney(v, cur), got.Basis
					addRat(&totals[k], v)
					estimatedBasis[k] = estimatedBasis[k] || got.Basis == "estimated"
					continue
				}
				*bases[k] = comps[k].Basis
				if ok && got.Basis != "" {
					*bases[k] = got.Basis
				}
			}
		}
		devices = append(devices, d)
	}
	homeBasis := func(k int) string {
		switch {
		case totals[k] != nil && estimatedBasis[k]:
			return "estimated"
		case totals[k] != nil:
			return "measured"
		case anyMetered:
			return comps[k].Basis
		case anyUnmetered:
			return "unmetered"
		case anyGas:
			return "fuel_not_applicable"
		}
		return comps[k].Basis
	}
	homeMoney := func(v *big.Rat) *money {
		if v == nil {
			return nil
		}
		return ratMoney(v, cur)
	}
	if meteredWaste == nil {
		meteredWaste = new(big.Rat)
	}
	optimal := new(big.Rat).Sub(cost, meteredWaste)
	if optimal.Sign() < 0 {
		optimal = new(big.Rat)
	}
	identified := new(big.Rat)
	var micros [4]int64
	for k, v := range totals {
		if v != nil {
			identified.Add(identified, v)
			micros[k] = moneyMicrosUnits(*ratMoney(v, cur))
		}
	}
	wasteBasis := "insufficient_data"
	switch {
	case wasteSum != nil && estimatedWaste:
		wasteBasis = "estimated"
	case wasteSum != nil:
		wasteBasis = "measured"
	case !anyMetered && anyUnmetered:
		wasteBasis = "unmetered"
	case !anyMetered && anyGas:
		wasteBasis = "fuel_not_applicable"
	}
	source := comps[1].Source
	if source == "" {
		source = "none"
	}
	away := homeMoney(totals[1])
	return &MeasuredEfficiency{
		Cost:                  in.Cost,
		IdentifiedOpportunity: *ratMoney(identified, cur),
		OptimalBase:           *ratMoney(optimal, cur),
		WasteOpportunity:      homeMoney(wasteSum),
		AwayOpportunity:       away,
		NightTotalOpportunity: homeMoney(totals[0]),
		AwayTotalOpportunity:  away,
		PeakRatesOpportunity:  homeMoney(totals[2]),
		HabitsOpportunity:     homeMoney(totals[3]),
		ChartSegmentWidth:     opportunityChartWidths(micros),
		WasteTimingBasis:      "measured_local_time",
		EvidenceBasis:         "measured",
		NightBasis:            homeBasis(0),
		AwayBasis:             homeBasis(1),
		PeakRatesBasis:        homeBasis(2),
		HabitsBasis:           homeBasis(3),
		WasteBasis:            wasteBasis,
		NightWindow:           EfficiencyNightWindow{StartLocal: nightStartLocal, EndLocal: nightEndLocal, Timezone: in.Loc.String()},
		AwaySource:            source,
		Devices:               devices,
	}
}
