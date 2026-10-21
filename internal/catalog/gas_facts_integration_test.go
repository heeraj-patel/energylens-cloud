package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestReadGasFactsAndGetSerialiseEveryKind(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_gas_facts_")
	ctx := context.Background()
	hash := strings.Repeat("c", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash); err != nil {
		t.Fatal(err)
	}
	product := seedBareProduct(t, pool, "natural_gas", "GD100A", "confirmed_exact")
	n := 0
	add := func(kind, unit string, value float64, basis, reviewed string, revision int) {
		t.Helper()
		n++
		factHash := fmt.Sprintf("%064x", n)
		fact, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "kind": kind, "value": value, "unit": unit})
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,'{}','other_official','https://example.org/f',$11,'reviewed_row_manifest','m',$8::timestamptz,$9,$10)`,
			uuid.NewString(), product, hash, kind, value, unit, basis, reviewed, revision, fact, factHash); err != nil {
			t.Fatalf("%s %s: %v", kind, unit, err)
		}
	}
	old, now := "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"
	add("annual_energy", "kWh", 55, "{}", now, 1)
	add("annual_fuel", "therm", 120, "{}", now, 1)
	add("annual_fuel", "gallon", 130, "{}", now, 1)
	add("combined_energy_factor", "lb/kWh", 2.9, `{"appendix":"D2"}`, old, 1)
	add("combined_energy_factor", "lb/kWh", 3.3, `{"appendix":"D1"}`, now, 2)
	add("uniform_energy_factor", "ratio", 0.81, `{"draw_pattern":"Medium"}`, now, 1)
	add("afue", "percent", 95, "{}", now, 1)
	add("standby_power", "W", 4.5, "{}", now, 1)
	add("off_mode_power", "W", 0, "{}", now, 1)
	add("auxiliary_power", "W", 60, "{}", now, 1)
	add("burner_input", "Btu/h", 22000, "{}", now, 1)

	got, err := ReadGasFacts(ctx, pool, product)
	if err != nil {
		t.Fatal(err)
	}
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if got.Fuel != "natural_gas" || f(got.AnnualFuelTherm) != 120 || f(got.AnnualFuelGallon) != 130 || f(got.AFUE) != 95 ||
		f(got.StandbyW) != 4.5 || f(got.OffModeW) != 0 || f(got.AuxiliaryW) != 60 || f(got.BurnerInputBtuH) != 22000 {
		t.Fatalf("scalar facts: %+v", got)
	}
	if got.CEF == nil || got.CEF.Value != 3.3 || got.CEF.TestBasis["appendix"] != "D1" {
		t.Fatalf("newest CEF not chosen: %+v", got.CEF)
	}
	if got.UEF == nil || got.UEF.Value != 0.81 || got.UEF.TestBasis["draw_pattern"] != "Medium" {
		t.Fatalf("UEF: %+v", got.UEF)
	}
	bare := seedBareProduct(t, pool, "propane", "GD200B", "confirmed_exact")
	if empty, err := ReadGasFacts(ctx, pool, bare); err != nil || empty.Fuel != "propane" || empty.CEF != nil || empty.AFUE != nil || empty.AnnualFuelGallon != nil {
		t.Fatalf("factless product: %+v %v", empty, err)
	}
	if _, err = ReadGasFacts(ctx, pool, uuid.NewString()); err != ErrNotFound {
		t.Fatalf("unknown product: %v", err)
	}

	body, err := (PGStore{Pool: pool}).Get(ctx, product)
	if err != nil {
		t.Fatal(err)
	}
	var served struct {
		Facts []struct {
			Kind  string
			Unit  string
			Value float64
		}
	}
	if err = json.Unmarshal([]byte(strings.ToLower(string(body))), &served); err != nil {
		t.Fatal(err)
	}
	units := map[string]bool{}
	for _, fact := range served.Facts {
		units[fact.Kind+"/"+fact.Unit] = true
	}
	for _, want := range []string{"annual_energy/kwh", "annual_fuel/therm", "annual_fuel/gallon", "combined_energy_factor/lb/kwh", "uniform_energy_factor/ratio", "afue/percent", "standby_power/w", "off_mode_power/w", "auxiliary_power/w", "burner_input/btu/h"} {
		if !units[want] {
			t.Errorf("Get omitted %s: %v", want, units)
		}
	}
}
