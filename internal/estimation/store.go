package estimation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PGRunStore struct{ Pool *pgxpool.Pool }

type applianceEvidence struct {
	ID        string
	Version   int64
	Category  string
	Brand     string
	Model     string
	Variant   *string
	Fuel      string
	Quality   string
	ProductID *string
}
type factEvidence struct {
	ID         string
	Revision   int64
	Value      string
	Count      int64
	Distinct   int64
	NonExact   int64
	SourceKind string
}
type rateEvidence struct {
	ID       string
	Revision int64
	Rate     string
	Currency string
	Kind     string
	Count    int64
}

func (s PGRunStore) ListRuns(ctx context.Context, owner, home string, appliance *string, now time.Time) ([]json.RawMessage, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var timezone string
	err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return nil, fmt.Errorf("invalid home timezone %q", timezone)
	}
	local := now.In(loc)
	start := fmt.Sprintf("%04d-%02d-01", local.Year(), local.Month())
	startTime := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	endTime := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	end := endTime.Format("2006-01-02")
	rows, err := tx.Query(ctx, `SELECT id,version,category,identity->>'brand',identity->>'full_model',identity->>'variant',identity->>'fuel',identity->>'match_quality',catalog_product_id
      FROM appliances WHERE home_id=$1 AND archived_at IS NULL AND ($2::uuid IS NULL OR id=$2)
      ORDER BY created_at,id`, home, appliance)
	if err != nil {
		return nil, err
	}
	items := []applianceEvidence{}
	for rows.Next() {
		var a applianceEvidence
		if err = rows.Scan(&a.ID, &a.Version, &a.Category, &a.Brand, &a.Model, &a.Variant, &a.Fuel, &a.Quality, &a.ProductID); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if appliance != nil && len(items) == 0 {
		return nil, ErrNotFound
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, a := range items {
		result, err := buildRun(ctx, tx, home, a, start, end, timezone, startTime, endTime, now)
		if err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func buildRun(ctx context.Context, tx pgx.Tx, home string, a applianceEvidence, start, end, timezone string, startTime, endTime, now time.Time) (json.RawMessage, error) {
	var err error
	usage, err := loadReviewedUsage(ctx, tx, home, a, start, end, timezone)
	if err != nil {
		return nil, err
	}
	var f factEvidence
	if a.Fuel == "electricity" && a.ProductID != nil {
		err := tx.QueryRow(ctx, `SELECT count(*),count(DISTINCT (value,test_basis,included_energy)),count(*) FILTER (WHERE source_kind NOT IN ('epa_exact','oem_exact')) FROM catalog_fact_revisions WHERE product_id=$1 AND kind='annual_energy' AND unit='kWh'`, *a.ProductID).Scan(&f.Count, &f.Distinct, &f.NonExact)
		if err != nil {
			return nil, err
		}
		if f.Count > 0 {
			err = tx.QueryRow(ctx, `SELECT id,revision,value::text,source_kind FROM catalog_fact_revisions WHERE product_id=$1 AND kind='annual_energy' AND unit='kWh' ORDER BY reviewed_at DESC,revision DESC,id DESC LIMIT 1`, *a.ProductID).Scan(&f.ID, &f.Revision, &f.Value, &f.SourceKind)
			if err != nil {
				return nil, err
			}
		}
	}
	var rate rateEvidence
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tariff_revisions WHERE home_id=$1`, home).Scan(&rate.Count); err != nil {
		return nil, err
	}
	if a.Fuel == "electricity" || a.Fuel == "natural_gas" {
		rateUnit := "kWh"
		if a.Fuel == "natural_gas" {
			rateUnit = "therm"
		}
		err = tx.QueryRow(ctx, `SELECT id,revision,rate::text,currency,rate_kind FROM tariff_revisions
      WHERE home_id=$1 AND fuel=$5 AND rate_unit=$6 AND effective_from <= $2
        AND (effective_to IS NULL OR effective_to >= $3)
        AND source_as_of >= $4::timestamptz - interval '18 months'
      ORDER BY CASE rate_kind WHEN 'user_tariff' THEN 0 WHEN 'official_tariff' THEN 1 ELSE 2 END,
               effective_from DESC,revision DESC,id DESC LIMIT 1`, home, startTime, endTime, now, a.Fuel, rateUnit).Scan(&rate.ID, &rate.Revision, &rate.Rate, &rate.Currency, &rate.Kind)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	quality := map[string]any{"identity": a.Quality, "consumption": "unavailable", "source": "unavailable", "context": "unknown", "rate": "unavailable"}
	refs := []map[string]any{}
	var unavailable string
	if a.Quality != "confirmed_exact" || a.ProductID == nil {
		unavailable = "identity_unresolved"
	}
	if usage.Selected {
		unavailable = ""
	}
	if usage.FactCount > 0 && !usage.Selected && (f.Count == 0 || usage.ExactScope) {
		unavailable = "basis_incomparable"
	}
	if unavailable == "" && a.Fuel != "electricity" && a.Fuel != "natural_gas" {
		unavailable = "basis_incomparable"
	}
	if unavailable == "" && f.Count == 0 && !usage.Selected {
		unavailable = "source_unreviewed"
	}
	if unavailable == "" && (f.Distinct > 1 || f.NonExact > 0) && !usage.Selected {
		unavailable = "basis_incomparable"
	}
	if f.Count > 0 && !usage.Selected {
		if f.SourceKind == "epa_exact" || f.SourceKind == "oem_exact" {
			quality["source"] = "reviewed_exact"
		} else {
			quality["source"] = "reviewed_family"
		}
		refs = append(refs, map[string]any{"id": f.ID, "kind": "catalog_fact", "revision": f.Revision})
	}
	if usage.Selected {
		quality["source"] = "reviewed_exact"
		refs = append(refs, map[string]any{"id": usage.FactID, "kind": "reviewed_usage_fact", "revision": 1})
	}
	if rate.ID != "" {
		quality["rate"] = rate.Kind
		refs = append(refs, map[string]any{"id": rate.ID, "kind": "tariff", "revision": rate.Revision})
	}
	if unavailable == "" && rate.ID == "" {
		unavailable = "rate_unavailable"
	}
	if unavailable == "" && rate.Currency != "USD" {
		unavailable = "basis_incomparable"
	}
	if unavailable == "" && usage.Selected && !usage.Confirmed {
		unavailable = "usage_unknown"
	}
	var calc Result
	if unavailable == "" {
		input := Input{Fact: Fact{Kind: "annual_energy", Unit: "kWh", Value: f.Value, Reviewed: true, Source: "reviewed_exact"}, Identity: "confirmed_exact", RatePerKWh: rate.Rate, RateKind: rate.Kind, StartDate: start, EndDate: end, Timezone: timezone}
		if usage.Selected {
			input.Fact = Fact{Kind: usage.Kind, Unit: usage.Unit, Fuel: a.Fuel, Value: usage.Value, Reviewed: true, Source: "reviewed_exact"}
			input.WeeklyUsage = &usage.Weekly
			if a.Fuel == "natural_gas" {
				input.RatePerKWh = ""
				input.RatePerTherm = rate.Rate
			}
		}
		calc, err = Calculate(input)
		if err != nil {
			return nil, err
		}
		quality["consumption"] = calc.ConsumptionQuality
	}
	// Count every immutable fact and tariff revision, including records that
	// cause no value change, so trigger invalidation always produces a new key.
	snapshot := map[string]any{"appliance_version": a.Version, "catalog_product_id": a.ProductID, "catalog_fact_id": f.ID, "catalog_fact_revision": f.Revision, "catalog_fact_source_kind": f.SourceKind, "catalog_fact_count": f.Count, "catalog_distinct_values": f.Distinct, "catalog_nonexact_sources": f.NonExact, "tariff_id": rate.ID, "tariff_revision": rate.Revision, "tariff_count": rate.Count, "confirmed_usage_revision": usage.AnswerRevision, "reviewed_usage_fact_id": usage.FactID, "reviewed_usage_publication_sha256": usage.PublicationHash, "reviewed_usage_fact_count": usage.FactCount, "reviewed_usage_status": usage.Status, "weekly_usage": usage.Weekly, "projected_usage": usage.Projected, "period_start": start, "period_end": end, "timezone": timezone, "formula_version": "reviewed-energy/v1"}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(snapshotJSON)
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT result_json FROM estimate_runs e WHERE home_id=$1 AND appliance_id=$2 AND period_start=$3 AND period_end=$4 AND dependency_hash=$5 AND NOT EXISTS(SELECT 1 FROM estimate_run_invalidations WHERE run_id=e.id)`, home, a.ID, start, end, hash[:]).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// Period or dependency change supersedes any previously exposed run.
	_, err = tx.Exec(ctx, `INSERT INTO estimate_run_invalidations(run_id,reason)
      SELECT id,CASE WHEN period_start<>$3 OR period_end<>$4 THEN 'period_changed' ELSE 'dependency_changed' END
      FROM estimate_runs e WHERE home_id=$1 AND appliance_id=$2
        AND NOT EXISTS(SELECT 1 FROM estimate_run_invalidations WHERE run_id=e.id)
	      ON CONFLICT (run_id) DO NOTHING`, home, a.ID, start, end)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	result := map[string]any{"id": runID, "appliance_id": a.ID, "kind": "unavailable", "period": map[string]any{"start": start, "end": end, "timezone": timezone}, "energy": nil, "cost": nil, "quality": quality, "waste": "not_yet_assessed", "evidence_refs": refs, "formula_version": "reviewed-energy/v1", "as_of": now.Format(time.RFC3339Nano)}
	if unavailable != "" {
		result["unavailable_reason"] = unavailable
	} else {
		result["kind"] = calc.Kind
		if a.Fuel == "natural_gas" {
			result["energy"] = map[string]any{"value": calc.EnergyTherms, "unit": "therm", "fuel": "natural_gas"}
		} else {
			result["energy"] = map[string]any{"value": calc.EnergyKWh, "unit": "kWh", "fuel": "electricity"}
		}
		result["cost"] = map[string]any{"value": calc.Cost, "currency": rate.Currency, "rate_kind": rate.Kind}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json,as_of)
      VALUES($1,$2,$3,$4,$5,$6,$7,'reviewed-energy/v1',$8,$9,$10,$11)`, runID, home, a.ID, a.Version, start, end, timezone, hash[:], snapshotJSON, body, now)
	return body, err
}
