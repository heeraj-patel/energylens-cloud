package datawizfacts

import (
	"strings"
	"testing"
)

const typicalDryer = `{"status":"resolved","source":{"tier":"category_typical"},"values":{"kwh_per_day":3},"waste_floor":{"usd_per_day":0.02}}`
const catalogRecord = `{"status":"resolved","source":{"tier":"catalog"},"values":{"kwh_per_day":0.1},"waste_floor":{"usd_per_day":0.02}}`

func TestForFuelElectricUnchanged(t *testing.T) {
	for _, fuel := range []string{"", "electricity"} {
		for _, rec := range []string{typicalDryer, catalogRecord} {
			out, ok, err := ForFuel(fuel, []byte(rec))
			if err != nil || !ok || string(out) != rec {
				t.Fatalf("%q: %s %v %v", fuel, out, ok, err)
			}
		}
	}
}

func TestForFuelGasDropsTypicalAndFloor(t *testing.T) {
	if _, ok, err := ForFuel("natural_gas", []byte(typicalDryer)); err != nil || ok {
		t.Fatalf("typical record served to gas appliance: %v %v", ok, err)
	}
	out, ok, err := ForFuel("propane", []byte(catalogRecord))
	if err != nil || !ok || strings.Contains(string(out), "waste_floor") || !strings.Contains(string(out), "kwh_per_day") {
		t.Fatalf("catalog record: %s %v %v", out, ok, err)
	}
}
