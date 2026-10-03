package questions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// reviewedCycleQuestion returns one prompt only when a single active fact
// covers this exact appliance identity and appliance-only cycle energy.
func reviewedCycleQuestion(ctx context.Context, db rowQuerier, home, appliance, category string, version int64) (*Question, error) {
	if category != "washer" && category != "dryer" && category != "dishwasher" {
		return nil, nil
	}
	var brand, model, fuel, quality string
	var variant *string
	err := db.QueryRow(ctx, `SELECT COALESCE(identity->>'brand',''),COALESCE(identity->>'full_model',''),identity->>'variant',COALESCE(identity->>'fuel',''),COALESCE(identity->>'match_quality','')
		FROM appliances WHERE id=$1 AND home_id=$2 AND version=$3 AND archived_at IS NULL`, appliance, home, version).
		Scan(&brand, &model, &variant, &fuel, &quality)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if quality != "confirmed_exact" || brand == "" || model == "" || variant == nil || strings.TrimSpace(*variant) == "" ||
		(fuel != "electricity" && !(category == "dryer" && fuel == "natural_gas")) {
		return nil, nil
	}
	var count int
	err = db.QueryRow(ctx, `SELECT count(*) FROM active_reviewed_usage_facts
		WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel=$4 AND variant=$5 AND measurement_kind='cycle_energy'`,
		category, brand, model, fuel, *variant).Scan(&count)
	if err != nil || count != 1 {
		return nil, err
	}
	var factID, unit, factQuality, scope, matched, sourceKind, sourceURL, basis string
	var value float64
	var cycle, mode *string
	var included []byte
	err = db.QueryRow(ctx, `SELECT id,value,unit,match_quality,documented_scope,matched_model,source_kind,source_url,test_basis,cycle_definition,operation_mode,included_energy
		FROM active_reviewed_usage_facts WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel=$4 AND variant=$5 AND measurement_kind='cycle_energy'`,
		category, brand, model, fuel, *variant).
		Scan(&factID, &value, &unit, &factQuality, &scope, &matched, &sourceKind, &sourceURL, &basis, &cycle, &mode, &included)
	if err != nil {
		return nil, err
	}
	if value <= 0 || factQuality != "confirmed_exact" || scope != model || matched != model ||
		(sourceKind != "oem_pdf" && sourceKind != "epa_row") || cycle == nil || strings.TrimSpace(*cycle) == "" ||
		strings.TrimSpace(basis) == "" || mode != nil || (fuel == "electricity" && unit != "kWh/cycle") ||
		(fuel == "natural_gas" && unit != "therm/cycle") || !cycleApplianceOnly(fuel, included) {
		return nil, nil
	}
	q := Question{ID: ReviewedCycleQuestionID(category, factID), Version: 1, Purpose: "onboarding",
		ApplianceID: appliance, IdentityRevision: version, AnswerKind: "cycles_per_week", Unit: "cycles/week", Optional: true,
		SourceFactID: factID, CycleDefinition: *cycle, TestBasis: basis, SourceURL: sourceURL,
		Prompt: fmt.Sprintf("For the reviewed %q cycle (test basis: %s), how many of those cycles do you usually run each week? Count other cycles separately.", *cycle, basis)}
	return &q, nil
}

func cycleApplianceOnly(fuel string, raw []byte) bool {
	var included struct {
		Status     string   `json:"status"`
		Components []string `json:"components"`
	}
	if json.Unmarshal(raw, &included) != nil || included.Status != "documented" || len(included.Components) != 1 {
		return false
	}
	if fuel == "natural_gas" {
		return included.Components[0] == "appliance_gas"
	}
	return included.Components[0] == "appliance_electricity"
}
