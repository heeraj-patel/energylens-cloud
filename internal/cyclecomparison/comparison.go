// Package cyclecomparison computes an ephemeral, owner-scoped comparison of
// one tagged dishwasher cycle with one reviewed laboratory cycle fact.
package cyclecomparison

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home or appliance not found")

type Request struct {
	Start                    time.Time `json:"start"`
	End                      time.Time `json:"end"`
	AttachmentID             string    `json:"attachment_id"`
	FactID                   string    `json:"fact_id"`
	ConfirmedTestBasis       string    `json:"confirmed_test_basis"`
	ConfirmedCycleDefinition string    `json:"confirmed_cycle_definition"`
	DedicatedNoOtherLoads    bool      `json:"dedicated_no_other_loads"`
}

type Result struct {
	Status                  string    `json:"status"`
	Reason                  string    `json:"reason,omitempty"`
	Start                   time.Time `json:"start"`
	End                     time.Time `json:"end"`
	FactID                  string    `json:"fact_id,omitempty"`
	MeasuredKWh             *float64  `json:"measured_kwh,omitempty"`
	LabReferenceKWhPerCycle *float64  `json:"lab_reference_kwh_per_cycle,omitempty"`
	DifferenceKWh           *float64  `json:"difference_kwh,omitempty"`
	Interpretation          string    `json:"interpretation"`
}

type Store struct{ Pool *pgxpool.Pool }

const interpretation = "Measured electricity for this tagged cycle compared with the reviewed source laboratory reference. Conditions may differ."

func unavailable(in Request, reason string) Result {
	return Result{Status: "unavailable", Reason: reason, Start: in.Start, End: in.End, Interpretation: interpretation}
}

// Compare reads a repeatable snapshot. A later correction requires a new call.
func (s Store) Compare(ctx context.Context, owner, home, appliance string, in Request, now time.Time) (Result, error) {
	out := unavailable(in, "insufficient_evidence")
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var category, identity string
	var revision int64
	err = tx.QueryRow(ctx, `SELECT a.category,a.identity::text,a.version FROM appliances a JOIN homes h ON h.id=a.home_id AND h.owner_user_id=$1 AND h.deleted_at IS NULL WHERE a.id=$2 AND a.home_id=$3 AND a.archived_at IS NULL`, owner, appliance, home).Scan(&category, &identity, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if category != "dishwasher" {
		return unavailable(in, "not_electric_dishwasher"), nil
	}
	var brand, model, variant, fuel, quality string
	err = tx.QueryRow(ctx, `SELECT COALESCE(identity->>'brand',''),COALESCE(identity->>'full_model',''),COALESCE(identity->>'variant',''),COALESCE(identity->>'fuel',''),COALESCE(identity->>'match_quality','') FROM appliances WHERE id=$1`, appliance).Scan(&brand, &model, &variant, &fuel, &quality)
	if err != nil {
		return out, err
	}
	if fuel != "electricity" || quality != "confirmed_exact" || strings.TrimSpace(brand) == "" || strings.TrimSpace(model) == "" {
		return unavailable(in, "identity_not_exact"), nil
	}
	if !in.DedicatedNoOtherLoads {
		return unavailable(in, "dedicated_use_unconfirmed"), nil
	}
	if in.Start.Before(now.Add(-30*24*time.Hour)) || in.End.After(now) {
		return unavailable(in, "outside_detail_retention"), nil
	}
	var sensor, hub, kind string
	var epoch, attachedRevision int64
	var attachedFrom time.Time
	var attachedUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT a.sensor_id,s.hub_id,s.ownership_epoch,s.kind,a.identity_revision,a.started_at,a.ended_at FROM attachments a JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id JOIN hubs u ON u.id=s.hub_id AND u.home_id=a.home_id AND u.ownership_epoch=s.ownership_epoch WHERE a.id=$1 AND a.home_id=$2 AND a.appliance_id=$3 AND s.archived_at IS NULL`, in.AttachmentID, home, appliance).Scan(&sensor, &hub, &epoch, &kind, &attachedRevision, &attachedFrom, &attachedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return unavailable(in, "attachment_unavailable"), nil
	}
	if err != nil {
		return out, err
	}
	if (kind != "plug" && kind != "strip_outlet") || attachedRevision != revision || attachedFrom.After(in.Start) || attachedUntil != nil && attachedUntil.Before(in.End) {
		return unavailable(in, "attachment_not_dedicated_for_window"), nil
	}
	var attachmentCount int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE sensor_id=$1 AND started_at<$3 AND COALESCE(ended_at,'infinity'::timestamptz)>$2`, sensor, in.Start, in.End).Scan(&attachmentCount)
	if err != nil {
		return out, err
	}
	if attachmentCount != 1 {
		return unavailable(in, "attachment_not_dedicated_for_window"), nil
	}
	// Inspect all matching facts, including an accidental duplicate introduced
	// outside the importer. Never choose a winner by row order.
	facts, err := tx.Query(ctx, `SELECT id,value::float8,test_basis,cycle_definition,included_energy FROM reviewed_usage_facts WHERE category='dishwasher' AND lower(manufacturer)=lower($1) AND regexp_replace(upper(model),'[^A-Z0-9]','','g')=regexp_replace(upper($2),'[^A-Z0-9]','','g') AND fuel='electricity' AND lower(variant)=lower($3) AND match_quality='confirmed_exact' AND regexp_replace(upper(documented_scope),'[^A-Z0-9]','','g')=regexp_replace(upper($2),'[^A-Z0-9]','','g') AND regexp_replace(upper(matched_model),'[^A-Z0-9]','','g')=regexp_replace(upper($2),'[^A-Z0-9]','','g') AND measurement_kind='cycle_energy' AND unit='kWh/cycle'`, brand, model, variant)
	if err != nil {
		return out, err
	}
	type fact struct {
		id           string
		value        float64
		basis, cycle string
		included     []byte
	}
	var selected []fact
	for facts.Next() {
		var f fact
		if err = facts.Scan(&f.id, &f.value, &f.basis, &f.cycle, &f.included); err != nil {
			facts.Close()
			return out, err
		}
		selected = append(selected, f)
	}
	err = facts.Err()
	facts.Close()
	if err != nil {
		return out, err
	}
	if len(selected) != 1 {
		return unavailable(in, "reviewed_fact_missing_or_ambiguous"), nil
	}
	f := selected[0]
	if f.id != in.FactID || f.basis != in.ConfirmedTestBasis || f.cycle != in.ConfirmedCycleDefinition {
		return unavailable(in, "reviewed_basis_not_confirmed"), nil
	}
	var electricOnly bool
	err = tx.QueryRow(ctx, `SELECT $1::jsonb->>'status'='documented' AND $1::jsonb->'components'='["appliance_electricity"]'::jsonb`, f.included).Scan(&electricOnly)
	if err != nil {
		return out, err
	}
	if !electricOnly {
		return unavailable(in, "fact_scope_not_appliance_electricity_only"), nil
	}
	// The latest revision is selected before interval filtering: a corrected
	// event moved out of the window must not leave its old revision visible.
	rows, err := tx.Query(ctx, `WITH latest AS (SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id) e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.event_kind,e.sensor_id FROM ingest_events e WHERE e.home_id=$1 AND e.hub_id=$3 AND e.ownership_epoch=$4 ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC) SELECT o.start_at,o.end_at,o.energy_wh,o.covered_seconds,o.attachment_revision_id,o.fuel,o.quality,o.source FROM latest l JOIN energy_observations o ON (o.hub_id,o.ownership_epoch,o.event_id,o.revision)=(l.hub_id,l.ownership_epoch,l.event_id,l.revision) WHERE l.sensor_id=$2 AND l.event_kind='energy_interval' AND o.start_at<$6 AND o.end_at>$5 ORDER BY o.start_at,o.end_at`, home, sensor, hub, epoch, in.Start, in.End)
	if err != nil {
		return out, err
	}
	var cursor = time.Time{}
	cursor = in.Start
	var wh float64
	count := 0
	for rows.Next() {
		var start, end time.Time
		var energy float64
		var covered int
		var aid, efuel, eq, source *string
		if err = rows.Scan(&start, &end, &energy, &covered, &aid, &efuel, &eq, &source); err != nil {
			rows.Close()
			return out, err
		}
		count++
		if !start.Equal(cursor) || end.After(in.End) || aid == nil || *aid != in.AttachmentID || efuel == nil || *efuel != "electricity" || eq == nil || *eq != "metered" || source == nil || *source != "verified_driver" || float64(covered) != end.Sub(start).Seconds() || energy < 0 || math.IsNaN(energy) || math.IsInf(energy, 0) {
			rows.Close()
			return unavailable(in, "raw_interval_coverage_or_scope_invalid"), nil
		}
		cursor = end
		wh += energy
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if count == 0 || !cursor.Equal(in.End) {
		return unavailable(in, "raw_interval_coverage_or_scope_invalid"), nil
	}
	var gaps int
	err = tx.QueryRow(ctx, `WITH latest AS (SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id) e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.event_kind,e.sensor_id FROM ingest_events e WHERE e.home_id=$1 AND e.hub_id=$3 AND e.ownership_epoch=$4 ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC) SELECT count(*) FROM latest l JOIN coverage_gaps g ON (g.hub_id,g.ownership_epoch,g.event_id,g.revision)=(l.hub_id,l.ownership_epoch,l.event_id,l.revision) WHERE l.sensor_id=$2 AND l.event_kind='coverage_gap' AND g.first_at<$6 AND g.last_at>$5`, home, sensor, hub, epoch, in.Start, in.End).Scan(&gaps)
	if err != nil {
		return out, err
	}
	if gaps > 0 {
		return unavailable(in, "coverage_gap_reported"), nil
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	measured := wh / 1000
	difference := measured - f.value
	return Result{Status: "available", Start: in.Start, End: in.End, FactID: f.id, MeasuredKWh: &measured, LabReferenceKWhPerCycle: &f.value, DifferenceKWh: &difference, Interpretation: interpretation}, nil
}
