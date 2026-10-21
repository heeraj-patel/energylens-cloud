package catalog

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// FactValue is one active catalog fact with the test_basis it was measured under.
type FactValue struct {
	Value     float64
	TestBasis map[string]any
}

// GasFacts holds a product's active gas-relevant facts, newest review per kind.
// A kind with no active fact is nil. Annual fuel is split by unit (therm, gallon).
type GasFacts struct {
	Fuel             string
	AnnualFuelTherm  *float64
	AnnualFuelGallon *float64
	CEF              *FactValue
	UEF              *FactValue
	AFUE             *float64
	StandbyW         *float64
	OffModeW         *float64
	AuxiliaryW       *float64
	BurnerInputBtuH  *float64
}

// ReadGasFacts returns the product's fuel and active gas-relevant facts.
// annual_energy (electric kWh) is not included; read it separately.
func ReadGasFacts(ctx context.Context, db catalogQueries, productID string) (GasFacts, error) {
	var out GasFacts
	err := db.QueryRow(ctx, `SELECT fuel FROM catalog_products WHERE id=$1`, productID).Scan(&out.Fuel)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (kind, unit) kind, unit, value::float8, test_basis
	    FROM active_catalog_facts
	    WHERE product_id=$1 AND kind IN ('annual_fuel','combined_energy_factor','uniform_energy_factor','afue','standby_power','off_mode_power','auxiliary_power','burner_input')
	    ORDER BY kind, unit, reviewed_at DESC, revision DESC, id DESC`, productID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, unit string
		var value float64
		var basis []byte
		if err = rows.Scan(&kind, &unit, &value, &basis); err != nil {
			return out, err
		}
		v := value
		switch kind {
		case "annual_fuel":
			if unit == "therm" {
				out.AnnualFuelTherm = &v
			} else {
				out.AnnualFuelGallon = &v
			}
		case "combined_energy_factor", "uniform_energy_factor":
			fv := &FactValue{Value: value, TestBasis: map[string]any{}}
			if err = json.Unmarshal(basis, &fv.TestBasis); err != nil {
				return out, err
			}
			if kind == "combined_energy_factor" {
				out.CEF = fv
			} else {
				out.UEF = fv
			}
		case "afue":
			out.AFUE = &v
		case "standby_power":
			out.StandbyW = &v
		case "off_mode_power":
			out.OffModeW = &v
		case "auxiliary_power":
			out.AuxiliaryW = &v
		case "burner_input":
			out.BurnerInputBtuH = &v
		}
	}
	return out, rows.Err()
}
