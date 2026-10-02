// Package estimation computes evidence-bounded operating and useful energy
// estimates. It never infers waste from an annual test figure or rated input.
package estimation

import (
	"errors"
	"math/big"
	"regexp"
	"time"
)

var ErrInvalidInput = errors.New("estimation: invalid input")
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

type Fact struct {
	Kind     string // annual_energy, cycle_energy, or hour_energy
	Unit     string // kWh, kWh/cycle, kWh/hour, or exact gas therm/cycle
	Fuel     string // empty/electricity, or natural_gas for exact therm/cycle evidence
	Value    string // positive decimal
	Reviewed bool
	Source   string // reviewed_exact or reviewed_family
}

type Input struct {
	Fact            Fact
	Identity        string  // confirmed_exact, documented_variant_family, unresolved
	Cycles          *int64  // confirmed cycles in the requested period; nil means unknown
	ProjectedCycles *string // decimal expected cycles projected from confirmed weekly frequency
	WeeklyUsage     *string // confirmed weekly cycles or hours; projected over exact calendar days
	Hours           *string // confirmed decimal hours in the requested period; nil means unknown
	RatePerKWh      string  // positive decimal; empty means unavailable
	RatePerTherm    string  // nonnegative decimal; only for natural_gas
	RateKind        string  // user_tariff, official_tariff, regional_average
	StartDate       string  // inclusive YYYY-MM-DD in Timezone
	EndDate         string  // exclusive YYYY-MM-DD in Timezone
	Timezone        string  // IANA name
}

type Result struct {
	Kind               string
	EnergyKWh          string
	EnergyTherms       string
	Cost               string
	Currency           string
	Waste              string
	ConsumptionQuality string
	UnavailableReason  string
	FormulaVersion     string
}

func number(raw string, allowZero bool) (*big.Rat, error) {
	if !decimal.MatchString(raw) {
		return nil, ErrInvalidInput
	}
	n, ok := new(big.Rat).SetString(raw)
	if !ok || (!allowZero && n.Sign() <= 0) {
		return nil, ErrInvalidInput
	}
	return n, nil
}

func day(raw string, location *time.Location) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil || parsed.Format("2006-01-02") != raw {
		return time.Time{}, ErrInvalidInput
	}
	return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 12, 0, 0, 0, location), nil
}

func annualFraction(start, end time.Time) *big.Rat {
	fraction := new(big.Rat)
	for date := start; date.Before(end); date = date.AddDate(0, 0, 1) {
		days := int64(365)
		if date.Year()%4 == 0 && (date.Year()%100 != 0 || date.Year()%400 == 0) {
			days = 366
		}
		fraction.Add(fraction, new(big.Rat).SetFrac64(1, days))
	}
	return fraction
}

// Calculate returns an operating estimate for a reviewed annual test figure,
// or a useful estimate for reviewed per-cycle/per-hour energy with confirmed
// matching usage.
// Annual tests are prorated by calendar days in each year, never converted to
// hourly/cycle draw. A zero confirmed cycle count is different from unknown.
func Calculate(in Input) (Result, error) {
	result := Result{Kind: "unavailable", Waste: "not_yet_assessed", FormulaVersion: "reviewed-energy/v1"}
	location, err := time.LoadLocation(in.Timezone)
	if err != nil || in.Timezone == "" || in.Timezone == "Local" {
		return Result{}, ErrInvalidInput
	}
	start, err := day(in.StartDate, location)
	if err != nil {
		return Result{}, err
	}
	end, err := day(in.EndDate, location)
	if err != nil || !end.After(start) || end.After(start.AddDate(10, 0, 0)) {
		return Result{}, ErrInvalidInput
	}
	if in.Fact.Kind != "annual_energy" && in.Fact.Kind != "cycle_energy" && in.Fact.Kind != "hour_energy" {
		return Result{}, ErrInvalidInput
	}
	gas := in.Fact.Fuel == "natural_gas"
	if in.Fact.Fuel != "" && in.Fact.Fuel != "electricity" && !gas {
		return Result{}, ErrInvalidInput
	}
	if gas && (in.Fact.Kind != "cycle_energy" || in.Fact.Unit != "therm/cycle") ||
		!gas && ((in.Fact.Kind == "annual_energy" && in.Fact.Unit != "kWh") ||
			(in.Fact.Kind == "cycle_energy" && in.Fact.Unit != "kWh/cycle") ||
			(in.Fact.Kind == "hour_energy" && in.Fact.Unit != "kWh/hour")) {
		return Result{}, ErrInvalidInput
	}
	fact, err := number(in.Fact.Value, false)
	if err != nil || (in.Cycles != nil && *in.Cycles < 0) || (in.Cycles != nil && (in.ProjectedCycles != nil || in.WeeklyUsage != nil)) || (in.ProjectedCycles != nil && in.WeeklyUsage != nil) || (in.Hours != nil && in.WeeklyUsage != nil) {
		return Result{}, ErrInvalidInput
	}
	var projectedCycles *big.Rat
	if in.ProjectedCycles != nil {
		projectedCycles, err = number(*in.ProjectedCycles, true)
		if err != nil {
			return Result{}, ErrInvalidInput
		}
	}
	var weeklyProjection *big.Rat
	if in.WeeklyUsage != nil {
		weeklyProjection, err = weeklyUsageFraction(*in.WeeklyUsage, start, end)
		if err != nil {
			return Result{}, err
		}
	}
	var hours *big.Rat
	if in.Hours != nil {
		hours, err = number(*in.Hours, true)
		if err != nil {
			return Result{}, ErrInvalidInput
		}
	}
	if in.Identity == "unresolved" {
		result.UnavailableReason = "identity_unresolved"
		return result, nil
	}
	if in.Identity != "confirmed_exact" && in.Identity != "documented_variant_family" {
		return Result{}, ErrInvalidInput
	}
	if gas && (in.Identity != "confirmed_exact" || in.Fact.Source != "reviewed_exact") {
		result.UnavailableReason = "basis_incomparable"
		return result, nil
	}
	if !in.Fact.Reviewed || (in.Fact.Source != "reviewed_exact" && in.Fact.Source != "reviewed_family") {
		result.UnavailableReason = "source_unreviewed"
		return result, nil
	}
	if (in.Fact.Kind == "cycle_energy" && in.Cycles == nil && projectedCycles == nil && weeklyProjection == nil) ||
		(in.Fact.Kind == "hour_energy" && hours == nil && weeklyProjection == nil) {
		result.UnavailableReason = "usage_unknown"
		return result, nil
	}
	rateValue := in.RatePerKWh
	if gas {
		rateValue = in.RatePerTherm
		if in.RatePerKWh != "" {
			return Result{}, ErrInvalidInput
		}
	} else if in.RatePerTherm != "" {
		return Result{}, ErrInvalidInput
	}
	if rateValue == "" {
		result.UnavailableReason = "rate_unavailable"
		return result, nil
	}
	if in.RateKind != "user_tariff" && in.RateKind != "official_tariff" && in.RateKind != "regional_average" {
		return Result{}, ErrInvalidInput
	}
	rate, err := number(rateValue, true)
	if err != nil {
		return Result{}, err
	}
	energy := new(big.Rat)
	if in.Fact.Kind == "annual_energy" {
		result.Kind = "estimated_operating_cost"
		result.ConsumptionQuality = "published_test_basis"
		energy.Mul(fact, annualFraction(start, end))
	} else if in.Fact.Kind == "cycle_energy" {
		result.Kind = "estimated_useful_cost"
		result.ConsumptionQuality = "modeled_user_usage"
		if weeklyProjection != nil {
			energy.Mul(fact, weeklyProjection)
		} else if projectedCycles != nil {
			energy.Mul(fact, projectedCycles)
		} else {
			energy.Mul(fact, new(big.Rat).SetInt64(*in.Cycles))
		}
	} else {
		result.Kind = "estimated_useful_cost"
		result.ConsumptionQuality = "modeled_user_usage"
		if weeklyProjection != nil {
			energy.Mul(fact, weeklyProjection)
		} else {
			energy.Mul(fact, hours)
		}
	}
	if gas {
		result.EnergyTherms = energy.FloatString(6)
	} else {
		result.EnergyKWh = energy.FloatString(6)
	}
	result.Cost = new(big.Rat).Mul(energy, rate).FloatString(6)
	result.Currency = "USD"
	return result, nil
}
