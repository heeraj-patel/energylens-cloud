package datawizrequests

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/inventory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	requeueLimit          = 50
	schemaV2              = "datawiz.energy-resolution.v2"
	day                   = 24 * time.Hour
	attemptsKept          = 90 * day
	categoryDatasetPrefix = "energy_categories:"
)

// Requeuer re-queues appliances whose stored resolution is low-tier, stale or out of date.
type Requeuer struct {
	DB  *pgxpool.Pool
	Now func() time.Time
}

type requeueRow struct {
	applianceID, homeID string
	identity            []byte
	catalogID           *string
	pBrand, pModel      *string
	pQuality            *string
	schema, status      string
	tier, recordKey     string
	recordURL           string
	datasetID, basis    string
	fingerprint         string
	resolvedAt          time.Time
	lastAttempt         *time.Time
	lowTierAttempts     int
	activeFacts         []string
}

// QueueWithAttempt queues the appliance and records the attempt in the caller's transaction.
func QueueWithAttempt(ctx context.Context, tx pgx.Tx, applianceID, homeID, reason string) (time.Time, error) {
	if err := Queue(ctx, tx, applianceID, reason); err != nil {
		return time.Time{}, err
	}
	return recordAttempt(ctx, tx, applianceID, homeID, reason)
}

func recordAttempt(ctx context.Context, tx pgx.Tx, applianceID, homeID, reason string) (time.Time, error) {
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT queued_at FROM datawiz_resolve_queue WHERE appliance_id=$1`, applianceID).Scan(&at); err != nil {
		return time.Time{}, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO datawiz_resolve_attempts(appliance_id,home_id,reason,queued_at) VALUES($1,$2,$3,$4)`, applianceID, homeID, reason, at)
	return at, err
}

func (q Requeuer) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// RunOnce queues at most 50 appliances for triggers R1, R1b, R2, R4 and R5, oldest record first.
func (q Requeuer) RunOnce(ctx context.Context) (int, error) {
	now := q.now()
	if _, err := q.DB.Exec(ctx, `DELETE FROM datawiz_resolve_attempts WHERE queued_at < $1`, now.Add(-attemptsKept)); err != nil {
		return 0, err
	}
	var current, categoryVersion string
	if err := q.DB.QueryRow(ctx, `SELECT COALESCE((SELECT fingerprint FROM datawiz_source_state),''),COALESCE((SELECT version::text FROM energy_category_catalog),'')`).Scan(&current, &categoryVersion); err != nil {
		return 0, err
	}
	rows, err := q.load(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range rows {
		if count >= requeueLimit {
			break
		}
		reason := r.autoReason(now, current, categoryVersion)
		if reason == "" {
			continue
		}
		queued, err := q.enqueue(ctx, r, reason)
		if err != nil {
			return count, err
		}
		if queued {
			count++
		}
	}
	return count, nil
}

// SchemaWave queues every live appliance whose stored record is older than v2 (R6).
func (q Requeuer) SchemaWave(ctx context.Context) (int, error) {
	rows, err := q.load(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range rows {
		if r.schema == schemaV2 {
			continue
		}
		queued, err := q.enqueue(ctx, r, "schema")
		if err != nil {
			return count, err
		}
		if queued {
			count++
		}
	}
	return count, nil
}

func (q Requeuer) enqueue(ctx context.Context, r requeueRow, reason string) (bool, error) {
	tx, err := q.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// An owner refresh queued meanwhile keeps its row, so insert only when absent.
	tag, err := tx.Exec(ctx, `INSERT INTO datawiz_resolve_queue(appliance_id,reason) VALUES($1,$2) ON CONFLICT(appliance_id) DO NOTHING`, r.applianceID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err = recordAttempt(ctx, tx, r.applianceID, r.homeID, reason); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (q Requeuer) load(ctx context.Context) ([]requeueRow, error) {
	rows, err := q.DB.Query(ctx, `SELECT a.id::text,a.home_id::text,a.identity,a.catalog_product_id::text,pr.brand,pr.full_model,pr.match_quality,
  COALESCE(r.record->>'schema_version',''),COALESCE(r.record->>'status',''),COALESCE(r.record->'source'->>'tier',''),
  COALESCE(r.record->'source'->>'record_key',''),COALESCE(r.record->'source'->>'url',''),COALESCE(r.record->'source'->>'dataset_id',''),COALESCE(r.record->'values'->>'waste_basis',''),
  COALESCE(r.record->>'source_fingerprint',''),r.resolved_at,
  (SELECT max(t.queued_at) FROM datawiz_resolve_attempts t WHERE t.appliance_id=a.id),
  (SELECT count(*) FROM datawiz_resolve_attempts t WHERE t.appliance_id=a.id AND t.reason='low_tier' AND t.queued_at>a.updated_at),
  COALESCE((SELECT array_agg(k) FROM (
    SELECT 'catalog:'||f.product_id||':'||f.revision||' '||f.source_url AS k FROM active_catalog_facts f WHERE f.product_id=a.catalog_product_id AND f.kind='annual_energy'
    UNION ALL
    SELECT 'catalog:'||pr.id||':rated:'||u.id||' '||u.source_url FROM reviewed_rated_input_facts u
     WHERE pr.match_quality='confirmed_exact' AND u.unit='W'
       AND regexp_replace(upper(u.manufacturer),'[^A-Z0-9]','','g')=regexp_replace(upper(pr.brand),'[^A-Z0-9]','','g')
       AND regexp_replace(upper(u.model),'[^A-Z0-9]','','g')=regexp_replace(upper(pr.full_model),'[^A-Z0-9]','','g')
       AND NOT EXISTS (SELECT 1 FROM reviewed_rated_input_revisions r WHERE r.prior_fact_id=u.id)) x),'{}')
 FROM appliances a JOIN homes h ON h.id=a.home_id
 JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||a.id AND r.record->'appliance_version'=to_jsonb(a.version)
 LEFT JOIN catalog_products pr ON pr.id=a.catalog_product_id
 WHERE a.archived_at IS NULL AND h.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM datawiz_resolve_queue w WHERE w.appliance_id=a.id)
 ORDER BY r.resolved_at,a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []requeueRow
	for rows.Next() {
		var r requeueRow
		if err = rows.Scan(&r.applianceID, &r.homeID, &r.identity, &r.catalogID, &r.pBrand, &r.pModel, &r.pQuality,
			&r.schema, &r.status, &r.tier, &r.recordKey, &r.recordURL, &r.datasetID, &r.basis, &r.fingerprint, &r.resolvedAt,
			&r.lastAttempt, &r.lowTierAttempts, &r.activeFacts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// autoReason returns the first automatic trigger that applies, or "".
func (r requeueRow) autoReason(now time.Time, fingerprint, categoryVersion string) string {
	sinceAttempt := func(gap time.Duration, withRecord bool) bool {
		base := time.Time{}
		if withRecord {
			base = r.resolvedAt
		}
		if r.lastAttempt != nil && r.lastAttempt.After(base) {
			base = *r.lastAttempt
		}
		return !now.Before(base.Add(gap))
	}
	if r.catalogChanged() && sinceAttempt(day, false) {
		return "catalog_change"
	}
	if r.sourceChanged(fingerprint, categoryVersion) && sinceAttempt(day, false) {
		return "source_config"
	}
	if r.status == "unresolved" || r.tier == "category_typical" {
		gap := 30 * day
		if r.lowTierAttempts < 3 {
			gap = min(7*day<<r.lowTierAttempts, gap)
		}
		if sinceAttempt(gap, true) {
			return "low_tier"
		}
	}
	if r.basis == "floor" && r.tier != "category_typical" && sinceAttempt(30*day, true) {
		return "low_tier"
	}
	if now.Sub(r.resolvedAt) > 30*day && sinceAttempt(30*day, true) {
		return "stale"
	}
	return ""
}

func (r requeueRow) sourceChanged(fingerprint, categoryVersion string) bool {
	if fingerprint != "" && r.fingerprint != fingerprint {
		return true
	}
	return r.tier == "category_typical" && categoryVersion != "" && strings.HasPrefix(r.datasetID, categoryDatasetPrefix) && r.datasetID != categoryDatasetPrefix+categoryVersion
}

func (r requeueRow) catalogChanged() bool {
	var identity inventory.Identity
	sendable := false
	if r.catalogID != nil && json.Unmarshal(r.identity, &identity) == nil {
		_, sendable = catalog.SendableLink(identity.Brand, identity.FullModel, identity.MatchQuality, catalog.LinkedProduct{Brand: deref(r.pBrand), FullModel: deref(r.pModel), MatchQuality: deref(r.pQuality)})
	}
	if !sendable {
		return r.tier == "catalog"
	}
	if len(r.activeFacts) == 0 {
		return r.tier == "catalog"
	}
	if r.tier != "catalog" && r.tier != "category_typical" && r.status != "unresolved" {
		return false
	}
	return !r.recordFactActive()
}

// recordFactActive reports whether the fact behind the stored record is still active.
// Facts of one product can share a revision number, so the source URL tells them apart.
func (r requeueRow) recordFactActive() bool {
	if r.recordURL == "" {
		return slices.ContainsFunc(r.activeFacts, func(k string) bool { return strings.HasPrefix(k, r.recordKey+" ") })
	}
	return slices.Contains(r.activeFacts, r.recordKey+" "+r.recordURL)
}
