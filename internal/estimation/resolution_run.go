package estimation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type energyResolution struct {
	Status     string `json:"status"`
	DeviceKind string `json:"device_kind"`
	Values     *struct {
		AnnualKWh      *float64 `json:"annual_kwh"`
		KWhPerDay      *float64 `json:"kwh_per_day"`
		WasteKWhPerDay *float64 `json:"waste_kwh_per_day"`
	} `json:"values"`
}

// buildResolutionRun snapshots the one Datawiz result and the applicable
// tariff. Source metadata never changes eligibility or arithmetic here.
func buildResolutionRun(ctx context.Context, tx pgx.Tx, home string, a applianceEvidence, start, end, timezone string, startTime, endTime, now time.Time) (json.RawMessage, error) {
	var metering bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments t JOIN sensor_endpoints s ON s.id=t.sensor_id WHERE t.appliance_id=$1 AND t.ended_at IS NULL AND s.archived_at IS NULL AND s.capabilities @> ARRAY['power','energy']::text[])`, a.ID).Scan(&metering); err != nil {
		return nil, err
	}
	var recordJSON []byte
	var requestHash []byte
	var resolvedAt time.Time
	err := tx.QueryRow(ctx, `SELECT record,request_sha256,resolved_at FROM datawiz_energy_resolutions WHERE appliance_ref=$1 AND record->'appliance_version'=to_jsonb($2::bigint)`, "cloud:"+a.ID, a.Version).Scan(&recordJSON, &requestHash, &resolvedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var resolution energyResolution
	if err == nil {
		if e := json.Unmarshal(recordJSON, &resolution); e != nil {
			return nil, e
		}
	}
	var rate rateEvidence
	if a.Fuel == "electricity" {
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM tariff_revisions WHERE home_id=$1`, home).Scan(&rate.Count); e != nil {
			return nil, e
		}
		err = tx.QueryRow(ctx, `SELECT id,revision,rate::text,currency,rate_kind FROM tariff_revisions
      WHERE home_id=$1 AND fuel='electricity' AND rate_unit='kWh' AND effective_from<=$2
        AND (effective_to IS NULL OR effective_to>=$3) AND source_as_of<=$4
        AND source_as_of>$4::timestamptz-interval '1440 hours'
      ORDER BY CASE rate_kind WHEN 'user_tariff' THEN 0 WHEN 'official_tariff' THEN 1 ELSE 2 END,
               effective_from DESC,revision DESC,id DESC LIMIT 1`, home, startTime, endTime, now).Scan(&rate.ID, &rate.Revision, &rate.Rate, &rate.Currency, &rate.Kind)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	quality := map[string]any{"identity": a.Quality, "consumption": "unavailable", "source": "unavailable", "context": "unknown", "rate": "unavailable"}
	refs := []map[string]any{}
	if len(recordJSON) > 0 {
		quality["source"] = "datawiz_resolution"
		refs = append(refs, map[string]any{"id": a.ID, "kind": "datawiz_resolution", "revision": 1})
	}
	if rate.ID != "" {
		quality["rate"] = rate.Kind
		refs = append(refs, map[string]any{"id": rate.ID, "kind": "tariff", "revision": rate.Revision})
	}
	var unavailable string
	if metering || len(recordJSON) == 0 || resolution.Status == "measured_device" || resolution.DeviceKind == "real_metering" || resolution.Values == nil {
		unavailable = "resolution_pending"
	} else if a.Fuel != "electricity" {
		unavailable = "basis_incomparable"
	} else if rate.ID == "" {
		unavailable = "rate_unavailable"
	} else if rate.Currency != "USD" {
		unavailable = "basis_incomparable"
	}
	var energy, waste float64
	if unavailable == "" {
		for day := startTime; day.Before(endTime); day = day.AddDate(0, 0, 1) {
			if resolution.Values.KWhPerDay != nil {
				energy += *resolution.Values.KWhPerDay
			} else if resolution.Values.AnnualKWh != nil {
				days := 365.0
				if day.Year()%4 == 0 && (day.Year()%100 != 0 || day.Year()%400 == 0) {
					days = 366
				}
				energy += *resolution.Values.AnnualKWh / days
			}
			if resolution.Values.WasteKWhPerDay != nil {
				waste += *resolution.Values.WasteKWhPerDay
			}
		}
		if math.IsNaN(energy) || math.IsInf(energy, 0) || energy <= 0 {
			unavailable = "resolution_pending"
		} else {
			quality["consumption"] = "resolved"
		}
	}
	snapshot := map[string]any{"appliance_version": a.Version, "metering_attachment": metering, "resolution_request_sha256": hex.EncodeToString(requestHash), "resolution_resolved_at": resolvedAt, "tariff_id": rate.ID, "tariff_revision": rate.Revision, "tariff_count": rate.Count, "period_start": start, "period_end": end, "timezone": timezone, "formula_version": "datawiz-resolution/v1"}
	snapshotJSON, e := json.Marshal(snapshot)
	if e != nil {
		return nil, e
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
	_, err = tx.Exec(ctx, `INSERT INTO estimate_run_invalidations(run_id,reason) SELECT id,CASE WHEN period_start<>$3 OR period_end<>$4 THEN 'period_changed' ELSE 'dependency_changed' END FROM estimate_runs e WHERE home_id=$1 AND appliance_id=$2 AND NOT EXISTS(SELECT 1 FROM estimate_run_invalidations WHERE run_id=e.id) ON CONFLICT(run_id) DO NOTHING`, home, a.ID, start, end)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	result := map[string]any{"id": runID, "appliance_id": a.ID, "kind": "unavailable", "period": map[string]any{"start": start, "end": end, "timezone": timezone}, "energy": nil, "cost": nil, "quality": quality, "waste": "not_yet_assessed", "evidence_refs": refs, "formula_version": "datawiz-resolution/v1", "as_of": now.Format(time.RFC3339Nano)}
	if unavailable != "" {
		result["unavailable_reason"] = unavailable
	} else {
		var rateValue float64
		if _, e = fmt.Sscan(rate.Rate, &rateValue); e != nil {
			return nil, e
		}
		result["kind"] = "estimated_operating_cost"
		result["energy"] = map[string]any{"value": fmt.Sprintf("%.6f", energy), "unit": "kWh", "fuel": "electricity"}
		result["cost"] = map[string]any{"value": fmt.Sprintf("%.6f", energy*rateValue), "currency": rate.Currency, "rate_kind": rate.Kind}
		if resolution.Values.WasteKWhPerDay != nil {
			result["waste_energy"] = map[string]any{"value": fmt.Sprintf("%.6f", waste), "unit": "kWh", "fuel": "electricity"}
			result["waste_cost"] = map[string]any{"value": fmt.Sprintf("%.6f", waste*rateValue), "currency": rate.Currency, "rate_kind": rate.Kind}
		}
	}
	body, e := json.Marshal(result)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json,as_of) VALUES($1,$2,$3,$4,$5,$6,$7,'datawiz-resolution/v1',$8,$9,$10,$11)`, runID, home, a.ID, a.Version, start, end, timezone, hash[:], snapshotJSON, body, now)
	return body, e
}
