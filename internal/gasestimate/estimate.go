// Package gasestimate estimates a gas-fuel appliance's electric kWh, gas therms
// and kWh-equivalent from catalog facts, falling back to cited typicals.
package gasestimate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"energylens.local/cloud/internal/catalog"
	"github.com/jackc/pgx/v5"
)

const (
	dryerCyclesPerYear    = 283.0  // DOE Appendix D1/D2 representative cycles per year
	dryerLoadLb           = 8.45   // DOE Appendix D1/D2 standard test load, lb
	dryerElectricKWhCycle = 0.21   // ORNL, April 1980, p. 36: blower, controls and tumbling
	typicalTanklessKWh    = 38.0   // CEC MAEDbS gas tankless median, 51 rows
	typicalStorageKWh     = 29.0   // CEC MAEDbS gas storage median, 116 rows
	btuPerKWh             = 3412.0 // 10 CFR 430 Appendix D2 4.4, Appendix E 6.3.5
	btuPerTherm           = 100000.0
	kWhEqPerTherm         = 29.31 // 100,000 / 3,412
	standbyHoursPerYear   = 8.76  // thousand h: (4,160 - BOH) + 4,600 h is below 8,760 (Appendix N 10.11)
	waterDensityLbPerGal  = 8.24  // 10 CFR 430 Appendix E 6.3.9
	waterSpecificHeat     = 1.00  // 10 CFR 430 Appendix E 6.3.9
	waterTempRiseF        = 67.0  // 10 CFR 430 Appendix E 6.3.9
	waterDrawDaysPerYear  = 365.0 // 10 CFR 430 Appendix E 6.3.9
	weeksPerYear          = 52.0
)

// Typical electric standby watts, used only when a gas appliance has no per-model figure.
const (
	typicalFurnaceStandbyW = 11.0 // DOE furnace NOPR 2022, Table IV.16 baseline
	typicalPoolStandbyW    = 7.2  // DOE pool heater final rule 2023, Table IV.3
	typicalBoilerStandbyW  = 9.0  // CEC MAEDbS sample, first 25 boiler rows
	typicalRangeStandbyW   = 3.1  // LBNL standby summary, cooking range median
	typicalDryerStandbyW   = 1.5  // ENERGY STAR dryer scoping report 2011
	typicalStorageStandbyW = 5.0  // NEEA 2021, power-vent storage water heater
)

// Basis values for Estimate.GasBasis and Estimate.ElectricBasis.
const (
	BasisRatedFact      = "rated_fact"
	BasisDerivedFromCEF = "derived_from_cef"
	BasisTypical        = "typical"
)

// drawGallons is the daily draw volume per UEF draw pattern (Appendix E 6.3.9).
var drawGallons = map[string]float64{"very_small": 10, "low": 38, "medium": 55, "high": 84}

// Class is the gas-appliance class an estimate rule applies to.
type Class string

const (
	ClassNone        Class = "none"
	ClassDryer       Class = "dryer"
	ClassWaterHeater Class = "water_heater"
	ClassBoiler      Class = "boiler"
	ClassFurnace     Class = "furnace"
	ClassSpaceHeater Class = "space_heater"
	ClassPoolHeater  Class = "pool_heater"
	ClassRange       Class = "range"
)

// Usage holds the owner's confirmed usage answers; nil is unanswered or "not sure".
// SeasonShare is the fraction of the year in use (0..1).
type Usage struct {
	CyclesPerWeek *float64
	HoursPerWeek  *float64
	SeasonShare   *float64
}

// Input is one appliance's class, fuel, active catalog facts and usage answers.
type Input struct {
	Class    Class
	Tankless bool
	Fuel     string
	Facts    catalog.GasFacts
	Usage    Usage
}

// Estimate holds yearly figures; a nil field is unknown. GasBasis is
// rated_fact, derived_from_cef or typical; ElectricBasis is rated_fact or typical.
// Propane keeps AnnualFuelGallon only: no therms, no kWh-eq, no cost.
type Estimate struct {
	// AnnualFuelBtu is a heating appliance's usage-based burner input, any gas fuel.
	AnnualFuelBtu *float64
	ElectricKWh      *float64
	GasTherms        *float64
	GasKWhEq         *float64
	TotalKWhEq       *float64
	AnnualFuelGallon *float64
	// StandbyKWh is the yearly standby: a per-model fact, else a cited typical.
	StandbyKWh *float64
	// StandbyBasis is rated_fact or typical.
	StandbyBasis  string
	GasBasis      string
	ElectricBasis string
}

// ClassOf maps a raw appliance category to a class. Tankless is true for a
// water heater whose category names a tankless (instantaneous) unit.
func ClassOf(category string) (Class, bool) {
	k := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(category)), "_")
	has := func(s ...string) bool {
		for _, v := range s {
			if strings.Contains(k, v) {
				return true
			}
		}
		return false
	}
	switch {
	case has("fireplace"):
		return ClassNone, false
	case has("pool"):
		return ClassPoolHeater, false
	case has("dryer"):
		return ClassDryer, false
	case has("water_heater", "waterheater", "tankless"):
		return ClassWaterHeater, has("tankless", "instantaneous", "on_demand")
	case has("boiler"):
		return ClassBoiler, false
	case has("furnace", "hvac"):
		return ClassFurnace, false
	case has("space_heater", "wall_heater", "floor_heater", "room_heater"):
		return ClassSpaceHeater, false
	case has("range", "oven", "cooktop", "cook_top", "stove"):
		return ClassRange, false
	}
	return ClassNone, false
}

func ptr(v float64) *float64 { return &v }

// WaterHeaterAnnualBtu is E_annual from UEF and draw pattern (Appendix E 6.3.9).
func WaterHeaterAnnualBtu(uef float64, drawPattern string) (float64, bool) {
	key := strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(drawPattern)))
	v, ok := drawGallons[key]
	if !ok || !(uef > 0) || math.IsInf(uef, 0) {
		return 0, false
	}
	return waterDrawDaysPerYear * v * waterDensityLbPerGal * waterSpecificHeat * waterTempRiseF / uef, true
}

// refine uses the facts where the category cannot tell: DOE files boilers and
// furnaces under hvac, and storage and tankless heaters under water_heater.
func refine(in Input) (Class, bool) {
	group := strings.ToLower(in.Facts.ProductGroup)
	class, tankless := in.Class, in.Tankless
	// Without a product group, standby/off-mode watts imply a boiler only when
	// no annual kWh fact (a furnace's) exists.
	wOnly := group == "" && in.Facts.AnnualEnergyKWh == nil && (in.Facts.StandbyW != nil || in.Facts.OffModeW != nil)
	if class == ClassFurnace && (strings.Contains(group, "boiler") || wOnly) {
		class = ClassBoiler
	}
	return class, tankless || class == ClassWaterHeater && strings.Contains(group, "instantaneous")
}

// Compute returns the estimate, or false for an electric appliance, which
// this package does not estimate.
func Compute(in Input) (Estimate, bool) {
	if in.Fuel == "" || in.Fuel == "electricity" {
		return Estimate{}, false
	}
	var e Estimate
	f := in.Facts
	class, tankless := refine(in)
	switch class {
	case ClassDryer:
		cycles := dryerCyclesPerYear
		if c := in.Usage.CyclesPerWeek; c != nil && validUsage(*c) {
			cycles = *c * weeksPerYear
		}
		e.ElectricKWh, e.ElectricBasis, e.GasBasis = ptr(dryerElectricKWhCycle*cycles), BasisTypical, BasisTypical
		e.setStandby(f, typicalDryerStandbyW)
		if f.CEF != nil && f.CEF.Value > 0 {
			eq := dryerLoadLb * cycles / f.CEF.Value
			e.GasBasis = BasisDerivedFromCEF
			e.setGas(in.Fuel, math.Max(0, eq-*e.ElectricKWh)*btuPerKWh/btuPerTherm, eq)
		}
	case ClassWaterHeater:
		typical := typicalStorageKWh
		if tankless {
			typical = typicalTanklessKWh
		}
		e.setElectric(f.AnnualEnergyKWh, typical)
		e.GasBasis = e.ElectricBasis
		if f.AnnualFuelTherm != nil {
			e.GasBasis = BasisRatedFact
			e.setGas(in.Fuel, *f.AnnualFuelTherm, 0)
		} else if f.UEF != nil {
			pattern, _ := f.UEF.TestBasis["draw_pattern"].(string)
			if btu, ok := WaterHeaterAnnualBtu(f.UEF.Value, pattern); ok {
				e.GasBasis = BasisRatedFact
				e.setGas(in.Fuel, math.Max(0, btu-btuPerKWh**e.ElectricKWh)/btuPerTherm, 0)
			}
		}
		if tankless {
			e.setStandby(f, 0)
		} else {
			e.setStandby(f, typicalStorageStandbyW)
		}
		if in.Fuel == "propane" && f.AnnualFuelGallon != nil {
			e.AnnualFuelGallon = ptr(*f.AnnualFuelGallon)
		}
	case ClassBoiler:
		if w, ok := maxPresent(f.StandbyW, f.OffModeW); ok {
			e.setElectric(ptr(w*standbyHoursPerYear), 0)
		}
		e.setStandby(f, typicalBoilerStandbyW)
		e.electricOnlyTotal()
		e.setHeatingGas(in)
	case ClassFurnace, ClassSpaceHeater, ClassPoolHeater, ClassRange:
		e.setElectric(f.AnnualEnergyKWh, 0)
		e.setStandby(f, map[Class]float64{ClassFurnace: typicalFurnaceStandbyW, ClassPoolHeater: typicalPoolStandbyW, ClassRange: typicalRangeStandbyW}[class])
		e.electricOnlyTotal()
		if class == ClassFurnace || class == ClassSpaceHeater {
			e.setHeatingGas(in)
		}
	}
	if e.GasBasis == "" {
		e.GasBasis = e.ElectricBasis
	}
	return e, true
}

func validUsage(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// setHeatingGas sets the yearly burner input from the rated Btu/h, hours per
// week and season share (1 when unanswered). Needs both the rated input and the hours.
func (e *Estimate) setHeatingGas(in Input) {
	btuH, hours := in.Facts.BurnerInputBtuH, in.Usage.HoursPerWeek
	if btuH == nil || hours == nil || !validUsage(*btuH) || !validUsage(*hours) {
		return
	}
	share := 1.0
	if s := in.Usage.SeasonShare; s != nil && validUsage(*s) {
		share = *s
	}
	btu := *btuH * *hours * weeksPerYear * share
	e.AnnualFuelBtu, e.GasBasis = ptr(btu), BasisRatedFact
	e.setGas(in.Fuel, btu/btuPerTherm, 0)
}

// setElectric uses the fact, else the typical when it is non-zero, else leaves nil.
func (e *Estimate) setElectric(fact *float64, typical float64) {
	switch {
	case fact != nil:
		e.ElectricKWh, e.ElectricBasis = ptr(*fact), BasisRatedFact
	case typical > 0:
		e.ElectricKWh, e.ElectricBasis = ptr(typical), BasisTypical
	}
}

// setStandby sets the yearly standby: the per-model standby or off-mode fact
// (the larger) wins, else the typical watts (0: none). With no electric figure, standby is the minimum electric use.
func (e *Estimate) setStandby(f catalog.GasFacts, typicalW float64) {
	switch w, ok := maxPresent(f.StandbyW, f.OffModeW); {
	case ok:
		e.StandbyKWh, e.StandbyBasis = ptr(w*standbyHoursPerYear), BasisRatedFact
	case typicalW > 0:
		e.StandbyKWh, e.StandbyBasis = ptr(typicalW*standbyHoursPerYear), BasisTypical
	}
	if e.ElectricKWh == nil && e.StandbyKWh != nil {
		e.ElectricKWh, e.ElectricBasis = ptr(*e.StandbyKWh), e.StandbyBasis
	}
}

// electricOnlyTotal is for classes with no gas estimate: total = electric.
func (e *Estimate) electricOnlyTotal() {
	if e.ElectricKWh != nil {
		e.TotalKWhEq = ptr(*e.ElectricKWh)
	}
}

// setGas records therms for natural gas only. A non-zero eq is the dryer's
// exact kWh-eq total; otherwise total = electric + therms x 29.31.
func (e *Estimate) setGas(fuel string, therms, eq float64) {
	if fuel != "natural_gas" {
		return
	}
	e.GasTherms = ptr(therms)
	var electric float64
	if e.ElectricKWh != nil {
		electric = *e.ElectricKWh
	}
	if eq > 0 {
		e.GasKWhEq, e.TotalKWhEq = ptr(eq-electric), ptr(eq)
		return
	}
	e.GasKWhEq = ptr(therms * kWhEqPerTherm)
	e.TotalKWhEq = ptr(electric + *e.GasKWhEq)
}

func maxPresent(a, b *float64) (float64, bool) {
	switch {
	case a != nil && b != nil:
		return math.Max(*a, *b), true
	case a != nil:
		return *a, true
	case b != nil:
		return *b, true
	}
	return 0, false
}

// Queries is the read surface of a pgx pool or transaction.
type Queries interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ForAppliance reads the linked product's facts (none when unlinked) and
// estimates with the usage answers. ok is false for an electric appliance.
func ForAppliance(ctx context.Context, db Queries, category, fuel string, productID *string, usage Usage) (Estimate, bool, error) {
	class, tankless := ClassOf(category)
	in := Input{Class: class, Tankless: tankless, Fuel: fuel, Usage: usage}
	if productID != nil && fuel != "" && fuel != "electricity" {
		facts, err := catalog.ReadGasFacts(ctx, db, *productID)
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return Estimate{}, false, err
		}
		in.Facts = facts
	}
	e, ok := Compute(in)
	return e, ok, nil
}

// ReadUsage returns the appliance's current-version usage answers. Only the
// latest revision of each onboarding question counts, and only when confirmed
// and in range; an unknown answer clears an earlier one.
func ReadUsage(ctx context.Context, db Queries, applianceID string, version int64) (Usage, error) {
	var u Usage
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (question_id) question_id,question_version,status,answer_value::text FROM question_answers WHERE appliance_id=$1 AND appliance_version=$2 ORDER BY question_id,revision DESC`, applianceID, version)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var qv int64
		var raw *string
		if err = rows.Scan(&id, &qv, &status, &raw); err != nil {
			return u, err
		}
		if status != "confirmed" || raw == nil || strings.Count(id, ".") != 1 || (qv != 1 && qv != 2) {
			continue
		}
		var v float64
		if err = json.Unmarshal([]byte(*raw), &v); err != nil || !validUsage(v) {
			continue
		}
		switch id[strings.LastIndex(id, ".")+1:] {
		case "cycles_per_week":
			if v <= 100 {
				u.CyclesPerWeek = &v
			}
		case "hours_per_week":
			if v <= 168 {
				u.HoursPerWeek = &v
			}
		case "season_share":
			if v <= 1 {
				u.SeasonShare = &v
			}
		}
	}
	return u, rows.Err()
}
