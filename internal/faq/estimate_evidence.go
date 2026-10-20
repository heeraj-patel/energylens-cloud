package faq

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var decimalAmount = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

// OperatingCostFromEstimate accepts only a current, sourced Cloud operating
// estimate for the requested appliance. It is a narrow bridge from the
// persisted estimate result; it never turns annual energy into useful cost.
func OperatingCostFromEstimate(raw json.RawMessage, applianceID string) (*OperatingCost, bool) {
	var result struct {
		ApplianceID    string `json:"appliance_id"`
		Kind           string `json:"kind"`
		FormulaVersion string `json:"formula_version"`
		Period         struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"period"`
		Cost *struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"cost"`
		Energy *struct {
			Value string `json:"value"`
			Unit  string `json:"unit"`
			Fuel  string `json:"fuel"`
		} `json:"energy"`
		Quality struct {
			Source      string `json:"source"`
			Consumption string `json:"consumption"`
		} `json:"quality"`
		Evidence []Evidence `json:"evidence_refs"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ApplianceID != applianceID || result.Kind != "estimated_operating_cost" || result.Cost == nil {
		return nil, false
	}
	start, err := time.Parse("2006-01-02", result.Period.Start)
	if err != nil {
		return nil, false
	}
	end, err := time.Parse("2006-01-02", result.Period.End)
	if err != nil || !end.After(start) {
		return nil, false
	}
	amount, ok := new(big.Rat).SetString(result.Cost.Value)
	if !decimalAmount.MatchString(result.Cost.Value) || !ok || amount.Sign() < 0 || len(result.Cost.Value) > 32 {
		return nil, false
	}
	currency := result.Cost.Currency
	if len(currency) != 3 || strings.ToUpper(currency) != currency {
		return nil, false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return nil, false
		}
	}
	var hasFact, hasResolution, hasRate bool
	for _, ref := range result.Evidence {
		if _, err := uuid.Parse(ref.ID); err != nil || ref.Revision < 1 {
			return nil, false
		}
		switch ref.Kind {
		case "catalog_fact":
			hasFact = true
		case "datawiz_resolution":
			if ref.ID != applianceID || ref.Revision != 1 {
				return nil, false
			}
			hasResolution = true
		case "tariff", "national_rate":
			hasRate = true
		default:
			return nil, false
		}
	}
	if !hasRate || (hasFact == hasResolution) {
		return nil, false
	}
	if hasResolution {
		if len(result.Evidence) != 2 || result.FormulaVersion != "datawiz-resolution/v1" || result.Quality.Source != "datawiz_resolution" || result.Quality.Consumption != "resolved" ||
			result.Energy == nil || result.Energy.Unit != "kWh" || result.Energy.Fuel != "electricity" ||
			!decimalAmount.MatchString(result.Energy.Value) || result.Cost.Currency != "USD" {
			return nil, false
		}
		if energy, ok := new(big.Rat).SetString(result.Energy.Value); !ok || energy.Sign() <= 0 {
			return nil, false
		}
	} else if result.Quality.Source == "datawiz_resolution" {
		return nil, false
	}
	return &OperatingCost{Amount: result.Cost.Value, Currency: currency, PeriodStart: result.Period.Start, PeriodEnd: result.Period.End, Evidence: result.Evidence}, true
}
