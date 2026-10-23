package gasestimate

import (
	"math"
	"testing"

	"energylens.local/cloud/internal/catalog"
)

func fp(v float64) *float64 { return &v }

func near(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > tol {
		t.Fatalf("%s = %v, want %v +-%v", name, got, want, tol)
	}
}

func isNil(t *testing.T, name string, got *float64) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %v, want nil", name, *got)
	}
}

func uef(v float64, pattern string) *catalog.FactValue {
	return &catalog.FactValue{Value: v, TestBasis: map[string]any{"draw_pattern": pattern}}
}

func TestGasDryerDVG45T6000VFromCEF(t *testing.T) {
	e, ok := Compute(Input{Class: ClassDryer, Fuel: "natural_gas", Facts: catalog.GasFacts{CEF: &catalog.FactValue{Value: 3.3, TestBasis: map[string]any{"appendix": "D1"}}}})
	if !ok {
		t.Fatal("gas dryer not estimated")
	}
	near(t, "total kWh-eq", e.TotalKWhEq, 724.7, 0.1)
	near(t, "electric", e.ElectricKWh, 59.4, 0.1)
	near(t, "therms", e.GasTherms, 22.7, 0.1)
	near(t, "gas kWh-eq", e.GasKWhEq, 724.7-59.4, 0.1)
	if e.GasBasis != BasisDerivedFromCEF || e.ElectricBasis != BasisTypical {
		t.Fatalf("basis %q/%q", e.GasBasis, e.ElectricBasis)
	}
}

func TestGasDryerWithoutCEFKeepsTypicalElectricOnly(t *testing.T) {
	e, _ := Compute(Input{Class: ClassDryer, Fuel: "natural_gas"})
	near(t, "electric", e.ElectricKWh, 59.43, 0.01)
	isNil(t, "therms", e.GasTherms)
	isNil(t, "total", e.TotalKWhEq)
	if e.GasBasis != BasisTypical {
		t.Fatalf("basis %q", e.GasBasis)
	}
}

func TestGasWaterHeaterUsesCECElectricAndENERGYSTARTherms(t *testing.T) {
	e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(47), AnnualFuelTherm: fp(135.2), UEF: uef(0.81, "medium")}})
	near(t, "electric", e.ElectricKWh, 47, 0)
	near(t, "therms", e.GasTherms, 135.2, 0)
	near(t, "total", e.TotalKWhEq, 47+135.2*29.31, 1e-9)
	if e.GasBasis != BasisRatedFact || e.ElectricBasis != BasisRatedFact {
		t.Fatalf("basis %q/%q", e.GasBasis, e.ElectricBasis)
	}
}

func TestGasWaterHeaterZeroElectricFactIsKept(t *testing.T) {
	e, _ := Compute(Input{Class: ClassWaterHeater, Tankless: true, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(0), AnnualFuelTherm: fp(100)}})
	near(t, "electric", e.ElectricKWh, 0, 0)
	near(t, "total", e.TotalKWhEq, 2931, 1e-9)
}

func TestGasWaterHeaterUEFDerivedKeepsAnnualBtu(t *testing.T) {
	btu, ok := WaterHeaterAnnualBtu(0.81, "medium")
	if !ok || math.Abs(btu-13682723) > 1 {
		t.Fatalf("E_annual = %v", btu)
	}
	e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(47), UEF: uef(0.81, "Medium")}})
	near(t, "therms", e.GasTherms, 135223.59/1000, 0.01)
	near(t, "fuel+electric Btu", fp(*e.GasTherms*btuPerTherm+*e.ElectricKWh*btuPerKWh), 13682723, 1)
	if e.GasBasis != BasisRatedFact {
		t.Fatalf("basis %q", e.GasBasis)
	}
}

func TestGasWaterHeaterTypicalsByType(t *testing.T) {
	facts := catalog.GasFacts{UEF: uef(0.81, "medium")}
	storage, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: facts})
	tankless, _ := Compute(Input{Class: ClassWaterHeater, Tankless: true, Fuel: "natural_gas", Facts: facts})
	near(t, "storage electric", storage.ElectricKWh, 29, 0)
	near(t, "tankless electric", tankless.ElectricKWh, 38, 0)
	if storage.ElectricBasis != BasisTypical {
		t.Fatalf("electric basis %q", storage.ElectricBasis)
	}
	none, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas"})
	isNil(t, "therms without facts", none.GasTherms)
	isNil(t, "total without facts", none.TotalKWhEq)
	if none.GasBasis != BasisTypical {
		t.Fatalf("basis %q", none.GasBasis)
	}
	if _, ok := WaterHeaterAnnualBtu(0.81, "huge"); ok {
		t.Fatal("unknown draw pattern accepted")
	}
}

func TestGasBoilerStandbyUpperBound(t *testing.T) {
	e, _ := Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: catalog.GasFacts{StandbyW: fp(9), OffModeW: fp(9)}})
	near(t, "electric", e.ElectricKWh, 78.84, 1e-9)
	near(t, "total", e.TotalKWhEq, 78.84, 1e-9)
	isNil(t, "therms", e.GasTherms)
	e, _ = Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: catalog.GasFacts{StandbyW: fp(4), OffModeW: fp(9)}})
	near(t, "max of standby and off", e.ElectricKWh, 78.84, 1e-9)
	near(t, "standby kWh follows the same bound", e.StandbyKWh, 78.84, 1e-9)
	if e.StandbyBasis != BasisRatedFact {
		t.Fatalf("standby basis %q", e.StandbyBasis)
	}
	e, _ = Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: catalog.GasFacts{OffModeW: fp(5)}})
	near(t, "off-mode only still estimates electric", e.ElectricKWh, 43.8, 1e-9)
	near(t, "off-mode only is a per-model figure", e.StandbyKWh, 43.8, 1e-9)
	if e.StandbyBasis != BasisRatedFact {
		t.Fatalf("standby basis %q", e.StandbyBasis)
	}
}

func TestGasOffModeOnlyStandbyIsPerModel(t *testing.T) {
	e, _ := Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: catalog.GasFacts{OffModeW: fp(3)}})
	near(t, "boiler off-mode 3 W", e.StandbyKWh, 26.28, 1e-9)
	if e.StandbyBasis != BasisRatedFact {
		t.Fatalf("basis %q", e.StandbyBasis)
	}
	e, _ = Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: catalog.GasFacts{OffModeW: fp(0)}})
	near(t, "boiler off-mode 0 W is rated zero", e.StandbyKWh, 0, 0)
	if e.StandbyBasis != BasisRatedFact {
		t.Fatalf("basis %q", e.StandbyBasis)
	}
	e, _ = Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{OffModeW: fp(4), ProductGroup: "Weatherized gas furnaces"}})
	near(t, "furnace off-mode", e.StandbyKWh, 35.04, 1e-9)
	if e.StandbyBasis != BasisRatedFact {
		t.Fatalf("basis %q", e.StandbyBasis)
	}
}

func TestGasStandbyTypicalsWithoutFacts(t *testing.T) {
	cases := []struct {
		name     string
		class    Class
		tankless bool
		kwh      float64
	}{
		{"furnace", ClassFurnace, false, 96.36}, {"pool heater", ClassPoolHeater, false, 63.072},
		{"boiler", ClassBoiler, false, 78.84}, {"range", ClassRange, false, 27.156},
		{"dryer", ClassDryer, false, 13.14}, {"storage water heater", ClassWaterHeater, false, 43.8},
	}
	for _, c := range cases {
		e, ok := Compute(Input{Class: c.class, Tankless: c.tankless, Fuel: "natural_gas"})
		if !ok || e.StandbyBasis != BasisTypical {
			t.Fatalf("%s: %+v", c.name, e)
		}
		near(t, c.name+" standby", e.StandbyKWh, c.kwh, 1e-9)
		if c.class != ClassDryer && c.class != ClassWaterHeater {
			near(t, c.name+" electric is the standby", e.ElectricKWh, c.kwh, 1e-9)
			near(t, c.name+" total", e.TotalKWhEq, c.kwh, 1e-9)
			if e.ElectricBasis != BasisTypical {
				t.Fatalf("%s electric basis %q", c.name, e.ElectricBasis)
			}
		}
	}
	d, _ := Compute(Input{Class: ClassDryer, Fuel: "natural_gas"})
	near(t, "dryer electric kept", d.ElectricKWh, 59.43, 0.01)
	w, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas"})
	near(t, "storage electric kept", w.ElectricKWh, 29, 0)
}

func TestGasStandbyNoneForTanklessHeaterFireplace(t *testing.T) {
	for _, in := range []Input{
		{Class: ClassWaterHeater, Tankless: true, Fuel: "natural_gas"}, {Class: ClassSpaceHeater, Fuel: "natural_gas"}, {Class: ClassNone, Fuel: "natural_gas"},
	} {
		e, _ := Compute(in)
		isNil(t, "standby", e.StandbyKWh)
		if in.Class != ClassWaterHeater {
			isNil(t, "electric", e.ElectricKWh)
		}
	}
	if c, _ := ClassOf("gas fireplace"); c != ClassNone {
		t.Fatalf("fireplace class %s", c)
	}
}

func TestGasStandbyFactBeatsTypical(t *testing.T) {
	for _, class := range []Class{ClassFurnace, ClassPoolHeater, ClassRange, ClassDryer, ClassWaterHeater, ClassBoiler} {
		e, _ := Compute(Input{Class: class, Fuel: "natural_gas", Facts: catalog.GasFacts{StandbyW: fp(2), ProductGroup: "x"}})
		near(t, string(class)+" fact standby", e.StandbyKWh, 17.52, 1e-9)
		if e.StandbyBasis != BasisRatedFact {
			t.Fatalf("%s basis %q", class, e.StandbyBasis)
		}
	}
	e, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(400)}})
	near(t, "furnace electric fact kept", e.ElectricKWh, 400, 0)
	near(t, "furnace typical standby", e.StandbyKWh, 96.36, 1e-9)
}

func TestGasSpaceHeaterElectricOnlyWhenFact(t *testing.T) {
	for _, class := range []Class{ClassSpaceHeater} {
		with, _ := Compute(Input{Class: class, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(341)}})
		near(t, string(class)+" electric", with.ElectricKWh, 341, 0)
		isNil(t, string(class)+" therms", with.GasTherms)
		without, _ := Compute(Input{Class: class, Fuel: "natural_gas"})
		isNil(t, string(class)+" electric without fact", without.ElectricKWh)
		isNil(t, string(class)+" total without fact", without.TotalKWhEq)
	}
}

func TestFireplaceClassIsAllNil(t *testing.T) {
	class, _ := ClassOf("fireplace")
	e, ok := Compute(Input{Class: class, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(10), StandbyW: fp(5)}})
	if !ok || e.ElectricKWh != nil || e.GasTherms != nil || e.TotalKWhEq != nil || e.StandbyKWh != nil {
		t.Fatalf("%+v", e)
	}
}

func TestPropaneKeepsGallonsAndNoTherms(t *testing.T) {
	e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "propane", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(0), AnnualFuelGallon: fp(210), AnnualFuelTherm: fp(190)}})
	near(t, "gallons", e.AnnualFuelGallon, 210, 0)
	near(t, "electric", e.ElectricKWh, 0, 0)
	isNil(t, "therms", e.GasTherms)
	isNil(t, "gas kWh-eq", e.GasKWhEq)
	isNil(t, "total", e.TotalKWhEq)
	d, _ := Compute(Input{Class: ClassDryer, Fuel: "propane", Facts: catalog.GasFacts{CEF: &catalog.FactValue{Value: 3.3}}})
	near(t, "propane dryer electric", d.ElectricKWh, 59.43, 0.01)
	isNil(t, "propane dryer therms", d.GasTherms)
}

func TestElectricApplianceIsNotEstimated(t *testing.T) {
	for _, fuel := range []string{"", "electricity"} {
		if e, ok := Compute(Input{Class: ClassDryer, Fuel: fuel}); ok || e != (Estimate{}) {
			t.Fatalf("fuel %q: %+v %v", fuel, e, ok)
		}
	}
}

func TestClassOf(t *testing.T) {
	cases := []struct {
		category string
		class    Class
		tankless bool
	}{
		{"dryer", ClassDryer, false}, {"Clothes Dryer", ClassDryer, false}, {"water_heater", ClassWaterHeater, false},
		{"Tankless Water Heater", ClassWaterHeater, true}, {"tankless", ClassWaterHeater, true}, {"boiler", ClassBoiler, false},
		{"furnace", ClassFurnace, false}, {"hvac", ClassFurnace, false}, {"space_heater", ClassSpaceHeater, false},
		{"pool heater", ClassPoolHeater, false}, {"oven", ClassRange, false}, {"Gas Range", ClassRange, false},
		{"cooktop", ClassRange, false}, {"fireplace", ClassNone, false}, {"refrigerator", ClassNone, false},
	}
	for _, c := range cases {
		if class, tankless := ClassOf(c.category); class != c.class || tankless != c.tankless {
			t.Errorf("%q = %s/%v, want %s/%v", c.category, class, tankless, c.class, c.tankless)
		}
	}
}

func TestHVACSplitsBoilerFromFurnaceByFacts(t *testing.T) {
	boiler, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{OffModeW: fp(0), ProductGroup: "Gas-fired hot water boiler"}})
	near(t, "boiler by group, off-mode 0 W", boiler.ElectricKWh, 0, 0)
	standby, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{StandbyW: fp(9)}})
	near(t, "boiler by standby fact", standby.ElectricKWh, 78.84, 1e-9)
	furnace, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{AFUE: fp(96), ProductGroup: "Weatherized gas furnaces"}})
	near(t, "furnace without kWh fact is typical standby", furnace.ElectricKWh, 96.36, 1e-9)
}

func TestInstantaneousProductGroupSelectsTanklessTypical(t *testing.T) {
	e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: catalog.GasFacts{ProductGroup: "Instantaneous Gas-fired Water Heater"}})
	near(t, "tankless typical", e.ElectricKWh, 38, 0)
	e, _ = Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: catalog.GasFacts{ProductGroup: "Gas-fired Storage Water Heater"}})
	near(t, "storage typical", e.ElectricKWh, 29, 0)
}

func TestFurnaceWithKWhFactAndStandbyStaysFurnace(t *testing.T) {
	e, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(400), StandbyW: fp(9), ProductGroup: "Weatherized gas furnaces"}})
	near(t, "furnace keeps annual kWh", e.ElectricKWh, 400, 1e-9)
	noGroup, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualEnergyKWh: fp(400), StandbyW: fp(9)}})
	near(t, "no group with kWh fact stays furnace", noGroup.ElectricKWh, 400, 1e-9)
}

func TestGasDryerUsesCyclesPerWeekAnswer(t *testing.T) {
	cef := catalog.GasFacts{CEF: &catalog.FactValue{Value: 3.3, TestBasis: map[string]any{"appendix": "D1"}}}
	e, _ := Compute(Input{Class: ClassDryer, Fuel: "natural_gas", Facts: cef, Usage: Usage{CyclesPerWeek: fp(4)}})
	near(t, "electric", e.ElectricKWh, 43.68, 1e-9)
	near(t, "total kWh-eq", e.TotalKWhEq, 8.45*208/3.3, 1e-9)
	near(t, "therms", e.GasTherms, (8.45*208/3.3-43.68)*3412/100000, 1e-9)
	noCEF, _ := Compute(Input{Class: ClassDryer, Fuel: "natural_gas", Usage: Usage{CyclesPerWeek: fp(4)}})
	near(t, "electric without CEF", noCEF.ElectricKWh, 43.68, 1e-9)
	isNil(t, "therms without CEF", noCEF.GasTherms)
	unanswered, _ := Compute(Input{Class: ClassDryer, Fuel: "natural_gas", Facts: cef, Usage: Usage{HoursPerWeek: fp(10), SeasonShare: fp(0.5)}})
	near(t, "unanswered total", unanswered.TotalKWhEq, 724.7, 0.1)
	near(t, "unanswered electric", unanswered.ElectricKWh, 59.4, 0.1)
	near(t, "unanswered therms", unanswered.GasTherms, 22.7, 0.1)
}

func TestGasHeatingFromHoursAndSeasonShare(t *testing.T) {
	facts := catalog.GasFacts{BurnerInputBtuH: fp(80000)}
	use := Usage{HoursPerWeek: fp(28), SeasonShare: fp(0.5)}
	for _, class := range []Class{ClassFurnace, ClassSpaceHeater} {
		e, _ := Compute(Input{Class: class, Fuel: "natural_gas", Facts: facts, Usage: use})
		near(t, string(class)+" therms", e.GasTherms, 582.4, 1e-9)
		near(t, string(class)+" btu", e.AnnualFuelBtu, 58240000, 1e-3)
		near(t, string(class)+" gas kWh-eq", e.GasKWhEq, 582.4*29.31, 1e-9)
		if e.GasBasis != BasisRatedFact {
			t.Fatalf("%s basis %q", class, e.GasBasis)
		}
	}
	furnace, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: facts, Usage: use})
	near(t, "furnace electric stays typical standby", furnace.ElectricKWh, 96.36, 1e-9)
	near(t, "furnace total", furnace.TotalKWhEq, 96.36+582.4*29.31, 1e-6)
	heater, _ := Compute(Input{Class: ClassSpaceHeater, Fuel: "natural_gas", Facts: facts, Usage: use})
	isNil(t, "space heater electric stays unknown", heater.ElectricKWh)
	near(t, "space heater total", heater.TotalKWhEq, 582.4*29.31, 1e-6)
	boiler, _ := Compute(Input{Class: ClassBoiler, Fuel: "natural_gas", Facts: facts, Usage: use})
	near(t, "boiler therms", boiler.GasTherms, 582.4, 1e-9)
	near(t, "boiler electric stays typical standby", boiler.ElectricKWh, 78.84, 1e-9)
}

func TestGasHeatingSeasonShareDefaultsToAllYear(t *testing.T) {
	e, _ := Compute(Input{Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{BurnerInputBtuH: fp(80000)}, Usage: Usage{HoursPerWeek: fp(28)}})
	near(t, "therms", e.GasTherms, 80000*28*52/100000.0, 1e-9)
}

func TestGasHeatingNeedsHoursAndBurnerInput(t *testing.T) {
	cases := map[string]Input{
		"no hours":  {Class: ClassFurnace, Fuel: "natural_gas", Facts: catalog.GasFacts{BurnerInputBtuH: fp(80000)}, Usage: Usage{SeasonShare: fp(0.5)}},
		"no burner": {Class: ClassFurnace, Fuel: "natural_gas", Usage: Usage{HoursPerWeek: fp(28)}},
		"no both":   {Class: ClassSpaceHeater, Fuel: "natural_gas"},
	}
	for name, in := range cases {
		e, _ := Compute(in)
		isNil(t, name+" therms", e.GasTherms)
		isNil(t, name+" btu", e.AnnualFuelBtu)
		if e.GasBasis == BasisRatedFact {
			t.Fatalf("%s basis %q", name, e.GasBasis)
		}
	}
}

func TestPropaneHeatingKeepsBtuWithoutThermsOrCost(t *testing.T) {
	e, _ := Compute(Input{Class: ClassFurnace, Fuel: "propane", Facts: catalog.GasFacts{BurnerInputBtuH: fp(80000)}, Usage: Usage{HoursPerWeek: fp(28), SeasonShare: fp(0.5)}})
	near(t, "btu", e.AnnualFuelBtu, 58240000, 1e-3)
	isNil(t, "therms", e.GasTherms)
	isNil(t, "gas kWh-eq", e.GasKWhEq)
	near(t, "gallons", e.AnnualFuelGallon, 58240000/91452.0, 1e-6)
	near(t, "total is electric only", e.TotalKWhEq, 96.36, 1e-9)
}

func TestElectricAppliancesIgnoreUsage(t *testing.T) {
	use := Usage{CyclesPerWeek: fp(4), HoursPerWeek: fp(28)}
	if e, ok := Compute(Input{Class: ClassFurnace, Fuel: "electricity", Facts: catalog.GasFacts{BurnerInputBtuH: fp(80000)}, Usage: use}); ok || e != (Estimate{}) {
		t.Fatalf("%+v %v", e, ok)
	}
	wh, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: catalog.GasFacts{AnnualFuelTherm: fp(135.2)}, Usage: use})
	near(t, "water heater therms", wh.GasTherms, 135.2, 0)
}

func TestGasWaterHeaterHouseholdSizeTherms(t *testing.T) {
	facts := catalog.GasFacts{AnnualEnergyKWh: fp(0), UEF: uef(0.58, "medium")}
	for gpd, therms := range map[float64]float64{38: 132, 64: 222.9, 91: 316.2} {
		e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: facts, Usage: Usage{HotWaterGalPerDay: fp(gpd)}})
		near(t, "therms", e.GasTherms, therms, 0.6)
		if e.GasBasis != BasisRatedFact {
			t.Fatalf("basis %q", e.GasBasis)
		}
	}
}

func TestGasWaterHeaterRatedThermsBeatHouseholdSize(t *testing.T) {
	facts := catalog.GasFacts{AnnualFuelTherm: fp(150), UEF: uef(0.58, "medium")}
	e, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: facts, Usage: Usage{HotWaterGalPerDay: fp(91)}})
	near(t, "therms", e.GasTherms, 150, 0)
}

func TestGasWaterHeaterNotSureKeepsDrawPattern(t *testing.T) {
	facts := catalog.GasFacts{AnnualEnergyKWh: fp(47), UEF: uef(0.81, "medium")}
	want, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: facts})
	got, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Facts: facts, Usage: Usage{CyclesPerWeek: fp(3)}})
	near(t, "therms", got.GasTherms, *want.GasTherms, 0)
	none, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas"})
	isNil(t, "therms without UEF", none.GasTherms)
}

func TestGasWaterHeaterHouseholdSizeDefaultUEF(t *testing.T) {
	use := Usage{HotWaterGalPerDay: fp(64)}
	storage, _ := Compute(Input{Class: ClassWaterHeater, Fuel: "natural_gas", Usage: use})
	tankless, _ := Compute(Input{Class: ClassWaterHeater, Tankless: true, Fuel: "natural_gas", Usage: use})
	btu := func(uef float64) float64 { return 365*64*8.24*67/uef - 3412*29 }
	near(t, "storage therms", storage.GasTherms, btu(0.5803)/100000, 1e-9)
	near(t, "tankless therms", tankless.GasTherms, (365*64*8.24*67/0.81-3412*38)/100000, 1e-9)
	if storage.GasBasis != BasisTypical || tankless.GasBasis != BasisTypical {
		t.Fatalf("basis %q/%q", storage.GasBasis, tankless.GasBasis)
	}
}
