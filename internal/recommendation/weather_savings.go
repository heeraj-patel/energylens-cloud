package recommendation

import (
	"cmp"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
)

const maxDeviceWeatherTips = 3

// minWeatherSaving is the monthly USD estimate a device weather tip must reach.
var (
	minWeatherSaving = big.NewRat(10, 1)
	weatherCostShare = big.NewRat(1, 10)
	tipNumber        = regexp.MustCompile(`^official:[^:]*:(W[0-9]+):`)
)

// tipDedupeKey is the tip's W<n> number from its record key; a key without
// one stands alone.
func tipDedupeKey(tip datawizfacts.Fact) string {
	if m := tipNumber.FindStringSubmatch(tip.RecordKey); m != nil {
		return m[1]
	}
	return tip.ID
}

func roundToCents(v *big.Rat) *big.Rat {
	cents := new(big.Rat).Mul(v, big.NewRat(100, 1))
	n := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(cents.Num(), big.NewInt(2)), cents.Denom()), new(big.Int).Mul(cents.Denom(), big.NewInt(2)))
	return new(big.Rat).SetFrac(n, big.NewInt(100))
}

// weatherEstimate is a device tip's monthly USD saving, or nil with no basis.
func weatherEstimate(a Appliance, v tipValue, trigger string, sv householdenergy.WeatherSavings) *big.Rat {
	device := sv.Devices[a.ID]
	if v.SavingsType == "load_shift" && device.Metered && device.MonthlyShiftSaving != nil {
		if sv.ShiftCurrency != "USD" {
			return nil
		}
		return roundToCents(device.MonthlyShiftSaving)
	}
	cost, currency := sv.ElectricMonthly, sv.ElectricCurrency
	if trigger == triggerColdAlert || trigger == triggerColdRun {
		cost, currency = sv.GasMonthly, sv.GasCurrency
	}
	if cost == nil || currency != "USD" {
		return nil
	}
	return roundToCents(new(big.Rat).Mul(cost, weatherCostShare))
}

type qualifiedTip struct {
	fact     datawizfacts.Fact
	estimate *big.Rat
	rank     int
}

// qualifyingWeatherTips returns the appliance's weather tips worth showing:
// active, matching its category and fuel, one per W<n>, at least $10 a month,
// best estimate first.
func qualifyingWeatherTips(a Appliance, tips []datawizfacts.Fact, w *WeatherState, sv householdenergy.WeatherSavings) []datawizfacts.Fact {
	var kept []qualifiedTip
	seen := map[string]bool{}
	for _, tip := range tips {
		v, ok := parseTip(tip)
		trigger := w.trigger(v.Context)
		if !ok || !isWeatherContext(v.Context) || trigger == "" || seen[tipDedupeKey(tip)] ||
			!tipApplies(v.Context, a.Category, "", v.Applicable, v.Excluded) || !tipFuelApplies(a.Fuel, v.Fuels) {
			continue
		}
		seen[tipDedupeKey(tip)] = true
		if est := weatherEstimate(a, v, trigger, sv); est != nil && est.Cmp(minWeatherSaving) >= 0 {
			kept = append(kept, qualifiedTip{tip, est, triggerRank(trigger)})
		}
	}
	slices.SortStableFunc(kept, func(x, y qualifiedTip) int {
		return cmp.Or(y.estimate.Cmp(x.estimate), x.rank-y.rank, strings.Compare(x.fact.RecordKey, y.fact.RecordKey))
	})
	out := make([]datawizfacts.Fact, 0, len(kept))
	for _, k := range kept {
		out = append(out, k.fact)
	}
	return out
}

// withoutWeatherTips drops weather-context tips, keeping the others in order.
func withoutWeatherTips(tips []datawizfacts.Fact) []datawizfacts.Fact {
	out := make([]datawizfacts.Fact, 0, len(tips))
	for _, tip := range tips {
		if v, ok := parseTip(tip); ok && isWeatherContext(v.Context) {
			continue
		}
		out = append(out, tip)
	}
	return out
}
