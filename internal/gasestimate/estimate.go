// Package gasestimate estimates a gas-fuel appliance's electric kWh, gas therms
// and kWh-equivalent from catalog facts, falling back to cited typicals.
package gasestimate

import (
	"context"
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
)

// Input is one appliance's class, fuel and active catalog facts.
type Input struct {
	Class    Class
	Tankless bool
	Fuel     string
	Facts    catalog.GasFacts
}

// Estimate holds yearly figures; a nil field is unknown. GasBasis is
// rated_fact, derived_from_cef or typical; ElectricBasis is rated_fact or typical.
// Propane keeps AnnualFuelGallon only: no therms, no kWh-eq, no cost.
type Estimate struct {
	ElectricKWh      *float64
	GasTherms        *float64
	GasKWhEq         *float64
	TotalKWhEq       *float64
	AnnualFuelGallon *float64
	// StandbyKWh is set only from a per-model standby_power fact (boilers).
	StandbyKWh    *float64
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
	case has("pool", "fireplace"):
		return ClassNone, false
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
	if class == ClassFurnace && (strings.Contains(group, "boiler") || in.Facts.StandbyW != nil || in.Facts.OffModeW != nil) {
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
		e.ElectricKWh, e.ElectricBasis, e.GasBasis = ptr(dryerElectricKWhCycle*dryerCyclesPerYear), BasisTypical, BasisTypical
		if f.CEF != nil && f.CEF.Value > 0 {
			eq := dryerLoadLb * dryerCyclesPerYear / f.CEF.Value
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
		if in.Fuel == "propane" && f.AnnualFuelGallon != nil {
			e.AnnualFuelGallon = ptr(*f.AnnualFuelGallon)
		}
	case ClassBoiler:
		if w, ok := maxPresent(f.StandbyW, f.OffModeW); ok {
			e.setElectric(ptr(w*standbyHoursPerYear), 0)
			if f.StandbyW != nil {
				e.StandbyKWh = ptr(w * standbyHoursPerYear)
			}
		}
		e.electricOnlyTotal()
	case ClassFurnace, ClassSpaceHeater:
		e.setElectric(f.AnnualEnergyKWh, 0)
		e.electricOnlyTotal()
	}
	if e.GasBasis == "" {
		e.GasBasis = e.ElectricBasis
	}
	return e, true
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
	if eq > 0 {
		e.GasKWhEq, e.TotalKWhEq = ptr(eq-*e.ElectricKWh), ptr(eq)
		return
	}
	e.GasKWhEq = ptr(therms * kWhEqPerTherm)
	e.TotalKWhEq = ptr(*e.ElectricKWh + *e.GasKWhEq)
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
// estimates. ok is false for an electric appliance.
func ForAppliance(ctx context.Context, db Queries, category, fuel string, productID *string) (Estimate, bool, error) {
	class, tankless := ClassOf(category)
	in := Input{Class: class, Tankless: tankless, Fuel: fuel}
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
