package estimation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type usageEvidence struct {
	FactID          string
	PublicationHash string
	FactCount       int64
	Kind            string
	Value           string
	Unit            string
	Selected        bool
	AnswerRevision  int64
	Status          string
	Confirmed       bool
	Projected       string
	Weekly          string
}

func usageQuestion(category string) (kind, question string) {
	switch category {
	case "washer", "dryer", "dishwasher":
		return "cycle_energy", category + ".cycles_per_week"
	case "computer", "television", "space_heater", "air_conditioner":
		return "hourly_energy", category + ".hours_per_week"
	default:
		return "", ""
	}
}

// loadReviewedUsage selects only a fully exact, operator-imported source for
// the current appliance identity. A family scope never silently binds itself
// to a household appliance, even if its pattern covers the model string.
func loadReviewedUsage(ctx context.Context, tx pgx.Tx, home string, a applianceEvidence, start, end, timezone string) (usageEvidence, error) {
	var out usageEvidence
	kind, question := usageQuestion(a.Category)
	if kind == "" || a.Quality != "confirmed_exact" || (a.Fuel != "electricity" && !(a.Fuel == "natural_gas" && a.Category == "dryer")) || a.Brand == "" || a.Model == "" || a.Variant == nil || strings.TrimSpace(*a.Variant) == "" {
		return out, nil
	}
	// Count all facts at this exact appliance identity, including rows whose
	// scope is not eligible. This keeps changed or ambiguous source evidence
	// from falling through to an unrelated annual calculation.
	err := tx.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_facts
		WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel=$4 AND variant=$5 AND measurement_kind=$6`,
		a.Category, a.Brand, a.Model, a.Fuel, *a.Variant, kind).Scan(&out.FactCount)
	if err != nil || out.FactCount == 0 {
		return out, err
	}
	if out.FactCount != 1 {
		return out, nil
	}
	var quality, scope, matched, sourceKind string
	err = tx.QueryRow(ctx, `SELECT id,publication_sha256,measurement_kind,value::text,unit,match_quality,documented_scope,matched_model,source_kind
		FROM reviewed_usage_facts WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel=$4 AND variant=$5 AND measurement_kind=$6`,
		a.Category, a.Brand, a.Model, a.Fuel, *a.Variant, kind).Scan(&out.FactID, &out.PublicationHash, &out.Kind, &out.Value, &out.Unit, &quality, &scope, &matched, &sourceKind)
	if err != nil {
		return out, err
	}
	if quality != "confirmed_exact" || scope != a.Model || matched != a.Model || (sourceKind != "oem_pdf" && sourceKind != "epa_row") {
		return out, nil
	}
	if a.Fuel == "electricity" && (out.Kind == "cycle_energy" && out.Unit != "kWh/cycle" || out.Kind == "hourly_energy" && out.Unit != "kWh/hour") ||
		a.Fuel == "natural_gas" && (out.Kind != "cycle_energy" || out.Unit != "therm/cycle") {
		return out, nil
	}
	if out.Kind == "hourly_energy" {
		out.Kind = "hour_energy"
	}
	out.Selected = true
	var answerValue []byte
	var answerVersion int64
	var questionVersion int
	err = tx.QueryRow(ctx, `SELECT revision,appliance_version,question_version,status,answer_value FROM question_answers
		WHERE home_id=$1 AND appliance_id=$2 AND question_id=$3
		ORDER BY revision DESC LIMIT 1`, home, a.ID, question).Scan(&out.AnswerRevision, &answerVersion, &questionVersion, &out.Status, &answerValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if answerVersion != a.Version || questionVersion != 1 || out.Status != "confirmed" {
		return out, nil
	}
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(answerValue))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return out, nil
	}
	weekly, ok := raw.(json.Number)
	if !ok {
		return out, nil
	}
	if kind == "cycle_energy" {
		cycles, err := strconv.ParseInt(weekly.String(), 10, 64)
		if err != nil || cycles < 0 || cycles > 100 {
			return out, nil
		}
	}
	projected, err := ProjectWeeklyUsage(weekly.String(), start, end, timezone)
	if err != nil {
		return out, nil
	}
	out.Projected = projected
	out.Weekly = weekly.String()
	out.Confirmed = true
	return out, nil
}
