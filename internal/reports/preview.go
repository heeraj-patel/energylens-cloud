// Package reports persists owner-scoped manual report previews. It never sends
// mail or copies home location, free-text inventory, answers or raw telemetry.
package reports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/recommendation"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("report preview or home not found")
var ErrInvalidSelection = errors.New("select 1 to 25 active appliances in this home")
var ErrKeyReuse = errors.New("idempotency key reused with a different report selection")
var decimalAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

type SourceRef struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

type Estimate struct {
	Kind         string      `json:"kind"`
	EnergyKWh    string      `json:"energy_kwh,omitempty"`
	CostUSD      string      `json:"cost_usd"`
	PeriodStart  string      `json:"period_start"`
	PeriodEnd    string      `json:"period_end"`
	AsOf         time.Time   `json:"as_of"`
	SourceRefs   []SourceRef `json:"source_refs"`
	EnergyTherms string      `json:"energy_therms,omitempty"`
	Fuel         string      `json:"fuel,omitempty"`
}

type MeasuredSummary struct {
	EnergyKWh    string    `json:"energy_kwh"`
	PeriodStart  string    `json:"period_start"`
	PeriodEnd    string    `json:"period_end"`
	BucketCount  int       `json:"bucket_count"`
	Source       string    `json:"source"`
	SourceSHA256 string    `json:"source_sha256"`
	SourceAsOf   time.Time `json:"source_as_of"`
	Coverage     string    `json:"coverage"`
}

type ReportRecommendation struct {
	ApplianceID  string      `json:"appliance_id"`
	RuleID       string      `json:"rule_id"`
	RuleVersion  int         `json:"rule_version"`
	State        string      `json:"state"`
	Title        string      `json:"title"`
	Explanation  string      `json:"explanation"`
	EvidenceRefs []SourceRef `json:"evidence_refs"`
}

// DeliveryStatus is the recipient-free latest request state for an immutable
// preview. It is read from the outbox and is not part of the preview digest.
type DeliveryStatus struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at"`
}

type Item struct {
	ApplianceID string           `json:"appliance_id"`
	Category    string           `json:"category"`
	Estimate    *Estimate        `json:"estimate"`
	Measured    *MeasuredSummary `json:"measured,omitempty"`
	Explanation string           `json:"explanation"`
}

type Preview struct {
	ID                    string                 `json:"id"`
	SnapshotSHA256        string                 `json:"snapshot_sha256"`
	HomeID                string                 `json:"home_id"`
	SchemaVersion         string                 `json:"schema_version"`
	CreatedAt             time.Time              `json:"created_at"`
	ExpiresAt             time.Time              `json:"expires_at"`
	Scope                 string                 `json:"scope"`
	HomeTotalStatus       string                 `json:"home_total_status"`
	MeasuredStatus        string                 `json:"measured_status"`
	RecommendationsStatus string                 `json:"recommendations_status"`
	Assumptions           []string               `json:"assumptions"`
	Items                 []Item                 `json:"items"`
	Recommendations       []ReportRecommendation `json:"recommendations,omitempty"`
	Stale                 bool                   `json:"stale"`
	LatestDelivery        *DeliveryStatus        `json:"latest_delivery"`
}

type Store struct{ DB *pgxpool.Pool }

func safeCategory(raw string) string {
	if raw != "" {
		return raw
	}
	return "other"
}

func snapshotDigest(preview Preview) (string, error) {
	preview.SnapshotSHA256 = ""
	preview.Stale = false
	body, err := json.Marshal(preview)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

type estimateResult struct {
	Kind           string `json:"kind"`
	FormulaVersion string `json:"formula_version"`
	Energy         *struct {
		Value string `json:"value"`
		Unit  string `json:"unit"`
		Fuel  string `json:"fuel"`
	} `json:"energy"`
	Cost *struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"cost"`
	Period struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"period"`
	Quality struct {
		Source      string `json:"source"`
		Consumption string `json:"consumption"`
	} `json:"quality"`
	EvidenceRefs []SourceRef `json:"evidence_refs"`
}

func vettedEstimate(raw []byte, applianceID, start, end string, asOf time.Time) *Estimate {
	var r estimateResult
	if json.Unmarshal(raw, &r) != nil || (r.Kind != "estimated_operating_cost" && r.Kind != "estimated_useful_cost") ||
		r.Energy == nil || r.Cost == nil || (r.Energy.Unit != "kWh" && r.Energy.Unit != "therm") || r.Cost.Currency != "USD" ||
		!decimalAmount.MatchString(r.Energy.Value) || !decimalAmount.MatchString(r.Cost.Value) ||
		r.Period.Start != start || r.Period.End != end ||
		(r.Quality.Source != "reviewed_exact" && r.Quality.Source != "datawiz_resolution") {
		return nil
	}
	hasFact, hasResolution, hasRate := false, false, false
	for _, ref := range r.EvidenceRefs {
		if _, err := uuid.Parse(ref.ID); err != nil || ref.Revision < 1 {
			return nil
		}
		switch ref.Kind {
		case "catalog_fact", "reviewed_usage_fact":
			hasFact = true
		case "datawiz_resolution":
			if ref.ID != applianceID || ref.Revision != 1 {
				return nil
			}
			hasResolution = true
		case "tariff", "national_rate":
			hasRate = true
		default:
			return nil
		}
	}
	if !hasRate || len(r.EvidenceRefs) > 4 ||
		(r.Quality.Source == "reviewed_exact" && (!hasFact || hasResolution)) ||
		(r.Quality.Source == "datawiz_resolution" && (!hasResolution || hasFact || r.Quality.Consumption != "resolved" || len(r.EvidenceRefs) != 2)) {
		return nil
	}
	if r.Quality.Source == "datawiz_resolution" {
		if r.FormulaVersion != "datawiz-resolution/v1" || r.Kind != "estimated_operating_cost" || r.Energy.Unit != "kWh" || r.Energy.Fuel != "electricity" {
			return nil
		}
		energy, ok := new(big.Rat).SetString(r.Energy.Value)
		if !ok || energy.Sign() <= 0 {
			return nil
		}
	}
	if r.Energy.Unit == "therm" {
		if r.Energy.Fuel != "natural_gas" || r.Kind != "estimated_useful_cost" {
			return nil
		}
		return &Estimate{Kind: r.Kind, EnergyTherms: r.Energy.Value, Fuel: "natural_gas", CostUSD: r.Cost.Value,
			PeriodStart: start, PeriodEnd: end, AsOf: asOf, SourceRefs: r.EvidenceRefs}
	}
	if r.Energy.Fuel != "" && r.Energy.Fuel != "electricity" {
		return nil
	}
	return &Estimate{Kind: r.Kind, EnergyKWh: r.Energy.Value, CostUSD: r.Cost.Value,
		PeriodStart: start, PeriodEnd: end, AsOf: asOf, SourceRefs: r.EvidenceRefs}
}

// Create copies only typed, allowlisted estimate fields. No report is sent.
// Missing or unsupported evidence remains explicit on each selected device.
func (s Store) Create(ctx context.Context, owner, home string, selected []uuid.UUID, key uuid.UUID, now time.Time) (Preview, error) {
	if s.DB == nil || key == uuid.Nil || len(selected) == 0 || len(selected) > 25 {
		return Preview{}, ErrInvalidSelection
	}
	unique := make(map[uuid.UUID]bool, len(selected))
	for _, id := range selected {
		if id == uuid.Nil || unique[id] {
			return Preview{}, ErrInvalidSelection
		}
		unique[id] = true
	}
	ordered := append([]uuid.UUID(nil), selected...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })
	canonical, err := json.Marshal(ordered)
	if err != nil {
		return Preview{}, err
	}
	hash := sha256.Sum256(canonical)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Preview{}, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var timezone string
	var rawDemo, demoDigest []byte
	err = tx.QueryRow(ctx, `SELECT version,timezone,demo_cost_snapshot,
		CASE WHEN demo_cost_snapshot IS NOT NULL THEN public.digest(demo_cost_snapshot::text,'sha256') END
		FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&version, &timezone, &rawDemo, &demoDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrNotFound
	}
	if err != nil {
		return Preview{}, err
	}
	var priorHash, priorBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,payload FROM report_previews WHERE owner_user_id=$1 AND home_id=$2 AND idempotency_key=$3`, owner, home, key).Scan(&priorHash, &priorBody)
	if err == nil {
		if !bytes.Equal(priorHash, hash[:]) {
			return Preview{}, ErrKeyReuse
		}
		var prior Preview
		if err = json.Unmarshal(priorBody, &prior); err != nil {
			return Preview{}, err
		}
		if err = tx.Rollback(ctx); err != nil {
			return Preview{}, err
		}
		return s.Get(ctx, owner, home, prior.ID, now)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, err
	}
	seeded := make(map[uuid.UUID]inventory.Appliance)
	if len(rawDemo) > 0 && string(rawDemo) != "null" {
		snapshot, decodeErr := householdenergy.DecodeDemoCostSnapshot(bytes.NewReader(rawDemo))
		if decodeErr != nil {
			return Preview{}, decodeErr
		}
		seededRows, deriveErr := inventory.DemoAppliances(snapshot, home)
		if deriveErr != nil {
			return Preview{}, deriveErr
		}
		for _, row := range seededRows {
			seeded[uuid.MustParse(row.ID)] = row
		}
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return Preview{}, fmt.Errorf("invalid home timezone")
	}
	local := now.In(loc)
	start := fmt.Sprintf("%04d-%02d-01", local.Year(), local.Month())
	endLocal := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	end := endLocal.Format("2006-01-02")
	startUTC := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).UTC()
	endUTC := endLocal.UTC()
	rows, err := tx.Query(ctx, `SELECT a.id,COALESCE(c.key,alias.category_key,'other'),a.version,e.id,e.result_json,e.as_of
		FROM appliances a LEFT JOIN LATERAL (
		 SELECT e.id,e.result_json,e.as_of FROM estimate_runs e
		 WHERE e.home_id=a.home_id AND e.appliance_id=a.id AND e.appliance_version=a.version
		   AND e.period_start=$3::date AND e.period_end=$4::date
		   AND NOT EXISTS(SELECT 1 FROM estimate_run_invalidations i WHERE i.run_id=e.id)
		 ORDER BY e.as_of DESC,e.id DESC LIMIT 1
		) e ON true
		LEFT JOIN energy_categories c ON c.key=a.category
		LEFT JOIN energy_category_aliases alias ON alias.alias=a.category
		WHERE a.home_id=$1 AND a.id=ANY($2::uuid[]) AND a.archived_at IS NULL ORDER BY a.id`, home, selected, start, end)
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{ID: uuid.NewString(), HomeID: home, SchemaVersion: "manual-preview/v2", CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(24 * time.Hour), Scope: "selected_appliances", HomeTotalStatus: "unavailable_incomplete_scope", MeasuredStatus: "unavailable", RecommendationsStatus: "unavailable", Assumptions: []string{"Per-device estimates use available energy data and a current monthly tariff.", "Measured electricity covers only complete daily sensor buckets for selected appliances; coverage may be incomplete.", "No whole-home total, measured savings, waste finding, or incentive is asserted."}, Items: []Item{}, Recommendations: []ReportRecommendation{}}
	type dependency struct {
		id         uuid.UUID
		version    *int64
		estimateID *uuid.UUID
		demoID     *string
	}
	deps := []dependency{}
	for rows.Next() {
		var id uuid.UUID
		var category string
		var applianceVersion int64
		var runID *uuid.UUID
		var body []byte
		var asOf *time.Time
		if err = rows.Scan(&id, &category, &applianceVersion, &runID, &body, &asOf); err != nil {
			break
		}
		item := Item{ApplianceID: id.String(), Category: safeCategory(category), Explanation: "No current monthly estimate is available for this appliance."}
		if runID != nil && asOf != nil {
			item.Estimate = vettedEstimate(body, id.String(), start, end, *asOf)
			if item.Estimate != nil {
				item.Explanation = "Monthly energy and cost estimate; this is not measured use or a savings claim."
			} else {
				runID = nil
			}
		}
		preview.Items = append(preview.Items, item)
		versionCopy := applianceVersion
		deps = append(deps, dependency{id: id, version: &versionCopy, estimateID: runID})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return Preview{}, err
	}
	for _, id := range ordered {
		row, isSeeded := seeded[id]
		if !isSeeded {
			continue
		}
		if len(deps) > 0 {
			for _, dep := range deps {
				if dep.id == id {
					return Preview{}, ErrInvalidSelection
				}
			}
		}
		demoID := row.DemoDeviceID
		preview.Items = append(preview.Items, Item{ApplianceID: row.ID, Category: safeCategory(row.Category),
			Explanation: "No current monthly estimate is available for this appliance."})
		deps = append(deps, dependency{id: id, demoID: &demoID})
	}
	if len(preview.Items) != len(selected) {
		return Preview{}, ErrInvalidSelection
	}
	for i := range preview.Items {
		if _, ok := seeded[uuid.MustParse(preview.Items[i].ApplianceID)]; ok {
			continue
		}
		var energyKWh string
		var buckets int
		var sensors int
		var sourceRows string
		var sourceAsOf *time.Time
		err = tx.QueryRow(ctx, `SELECT COALESCE(sum(d.energy_wh)/1000,0)::text,count(*)::int,count(DISTINCT d.sensor_id)::int,
			COALESCE(jsonb_agg(jsonb_build_object('hub',d.hub_id,'epoch',d.ownership_epoch,'sensor',d.sensor_id,'attachment',d.attachment_id,'bucket',d.bucket_start,'energy_wh',d.energy_wh,'covered_seconds',d.covered_seconds,'allocation',d.allocation_method,'refs',d.source_refs,'computed_at',d.computed_at) ORDER BY d.bucket_start,d.sensor_id,d.attachment_id),'[]'::jsonb)::text,
			max(d.computed_at) FROM energy_daily d
			JOIN attachments a ON a.id=d.attachment_id AND a.home_id=d.home_id
			WHERE d.home_id=$1 AND a.appliance_id=$2 AND d.status='complete'
			AND d.bucket_start >= $3::timestamptz
			AND d.bucket_start + interval '1 day' <= LEAST($4::timestamptz,$5::timestamptz)`,
			home, preview.Items[i].ApplianceID, startUTC, endUTC, now.UTC()).Scan(&energyKWh, &buckets, &sensors, &sourceRows, &sourceAsOf)
		if err != nil {
			return Preview{}, err
		}
		if buckets > 0 && sensors == 1 {
			sourceHash := sha256.Sum256([]byte(sourceRows))
			preview.Items[i].Measured = &MeasuredSummary{EnergyKWh: energyKWh, PeriodStart: start, PeriodEnd: end,
				BucketCount: buckets, Source: "energy_daily", SourceSHA256: hex.EncodeToString(sourceHash[:]), SourceAsOf: *sourceAsOf, Coverage: "partial_or_unknown"}
			preview.MeasuredStatus = "partial"
		}
	}
	sort.Slice(preview.Items, func(i, j int) bool { return preview.Items[i].ApplianceID < preview.Items[j].ApplianceID })
	// The shared evaluator supplies only bounded actions. It never computes savings.
	recommendations := recommendation.PGStore{Pool: s.DB}
	current, err := recommendations.Current(ctx, owner, home, now)
	if err != nil {
		return Preview{}, err
	}
	events, err := recommendations.Latest(ctx, owner, home)
	if err != nil {
		return Preview{}, err
	}
	actions := []recommendation.Action{}
	for _, appliance := range current {
		if !unique[uuid.MustParse(appliance.ID)] {
			continue
		}
		actions = append(actions, recommendation.EvaluateGasEnabled(appliance)...)
	}
	for _, action := range recommendation.ApplyLifecycle(actions, events, now) {
		if action.State != "active" && action.State != "candidate" && action.State != "accepted" {
			continue
		}
		refs := make([]SourceRef, 0, len(action.EvidenceRefs))
		for _, ref := range action.EvidenceRefs {
			refs = append(refs, SourceRef{ID: ref.ID, Kind: ref.Kind, Revision: ref.Revision})
		}
		preview.Recommendations = append(preview.Recommendations, ReportRecommendation{ApplianceID: action.ApplianceID,
			RuleID: action.RuleID, RuleVersion: action.RuleVersion, State: action.State,
			Title: action.Title, Explanation: action.Explanation, EvidenceRefs: refs})
	}
	if len(preview.Recommendations) > 0 {
		preview.RecommendationsStatus = "available"
	}
	sort.Slice(preview.Recommendations, func(i, j int) bool {
		a, b := preview.Recommendations[i], preview.Recommendations[j]
		if a.ApplianceID != b.ApplianceID {
			return a.ApplianceID < b.ApplianceID
		}
		return a.RuleID < b.RuleID
	})
	preview.SnapshotSHA256, err = snapshotDigest(preview)
	if err != nil {
		return Preview{}, err
	}
	body, err := json.Marshal(preview)
	if err != nil {
		return Preview{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO report_previews(id,owner_user_id,home_id,home_version,idempotency_key,request_hash,schema_version,payload,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, preview.ID, owner, home, version, key, hash[:], preview.SchemaVersion, body, preview.CreatedAt, preview.ExpiresAt)
	if err != nil {
		return Preview{}, err
	}
	for _, d := range deps {
		var dependencyDigest []byte
		if d.demoID != nil {
			dependencyDigest = demoDigest
		}
		_, err = tx.Exec(ctx, `INSERT INTO report_preview_items(preview_id,appliance_id,appliance_version,estimate_run_id,demo_device_id,demo_snapshot_sha256)
			VALUES($1,$2,$3,$4,$5,$6)`, preview.ID, d.id, d.version, d.estimateID, d.demoID, dependencyDigest)
		if err != nil {
			return Preview{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Preview{}, err
	}
	return preview, nil
}

// Get returns an immutable preview with a computed stale marker. It remains
// readable until expiry but a later send flow must reject stale previews.
func (s Store) Get(ctx context.Context, owner, home, previewID string, now time.Time) (Preview, error) {
	var body []byte
	var expires time.Time
	var homeVersion int64
	err := s.DB.QueryRow(ctx, `SELECT p.payload,p.expires_at,p.home_version FROM report_previews p
		JOIN homes h ON h.id=p.home_id AND h.owner_user_id=p.owner_user_id AND h.deleted_at IS NULL
		WHERE p.id=$1 AND p.owner_user_id=$2 AND p.home_id=$3`, previewID, owner, home).Scan(&body, &expires, &homeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrNotFound
	}
	if err != nil {
		return Preview{}, err
	}
	var out Preview
	if err = json.Unmarshal(body, &out); err != nil {
		return Preview{}, err
	}
	var stale bool
	err = s.DB.QueryRow(ctx, dependencyStaleSQL, previewID, home, homeVersion).Scan(&stale)
	if err != nil {
		return Preview{}, err
	}
	out.Stale = stale || !now.Before(expires)
	var delivery DeliveryStatus
	var sentAt *time.Time
	err = s.DB.QueryRow(ctx, `SELECT id,state,created_at,sent_at FROM report_delivery_requests
		WHERE owner_user_id=$1 AND home_id=$2 AND preview_id=$3
		ORDER BY created_at DESC,id DESC LIMIT 1`, owner, home, previewID).
		Scan(&delivery.ID, &delivery.State, &delivery.CreatedAt, &sentAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, err
	}
	if err == nil {
		delivery.SentAt = sentAt
		out.LatestDelivery = &delivery
	}
	return out, nil
}
