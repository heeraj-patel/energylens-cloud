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
	"regexp"
	"sort"
	"time"

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
	Kind        string      `json:"kind"`
	EnergyKWh   string      `json:"energy_kwh"`
	CostUSD     string      `json:"cost_usd"`
	PeriodStart string      `json:"period_start"`
	PeriodEnd   string      `json:"period_end"`
	AsOf        time.Time   `json:"as_of"`
	SourceRefs  []SourceRef `json:"source_refs"`
}

type Item struct {
	ApplianceID string    `json:"appliance_id"`
	Category    string    `json:"category"`
	Estimate    *Estimate `json:"estimate"`
	Explanation string    `json:"explanation"`
}

type Preview struct {
	ID                    string    `json:"id"`
	SnapshotSHA256        string    `json:"snapshot_sha256"`
	HomeID                string    `json:"home_id"`
	SchemaVersion         string    `json:"schema_version"`
	CreatedAt             time.Time `json:"created_at"`
	ExpiresAt             time.Time `json:"expires_at"`
	Scope                 string    `json:"scope"`
	HomeTotalStatus       string    `json:"home_total_status"`
	MeasuredStatus        string    `json:"measured_status"`
	RecommendationsStatus string    `json:"recommendations_status"`
	Assumptions           []string  `json:"assumptions"`
	Items                 []Item    `json:"items"`
	Stale                 bool      `json:"stale"`
}

type Store struct{ DB *pgxpool.Pool }

var reportCategories = map[string]bool{
	"refrigerator": true, "freezer": true, "washer": true, "dryer": true, "dishwasher": true,
	"water_heater": true, "air_conditioner": true, "heat_pump": true, "space_heater": true,
	"computer": true, "television": true, "range": true, "oven": true, "microwave": true,
	"ev_charger": true, "solar": true, "storage": true, "other": true,
}

func safeCategory(raw string) string {
	if reportCategories[raw] {
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
	Kind   string `json:"kind"`
	Energy *struct {
		Value string `json:"value"`
		Unit  string `json:"unit"`
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
		Source string `json:"source"`
	} `json:"quality"`
	EvidenceRefs []SourceRef `json:"evidence_refs"`
}

func vettedEstimate(raw []byte, start, end string, asOf time.Time) *Estimate {
	var r estimateResult
	if json.Unmarshal(raw, &r) != nil || (r.Kind != "estimated_operating_cost" && r.Kind != "estimated_useful_cost") ||
		r.Energy == nil || r.Cost == nil || r.Energy.Unit != "kWh" || r.Cost.Currency != "USD" ||
		!decimalAmount.MatchString(r.Energy.Value) || !decimalAmount.MatchString(r.Cost.Value) ||
		r.Period.Start != start || r.Period.End != end || r.Quality.Source != "reviewed_exact" {
		return nil
	}
	hasFact, hasTariff := false, false
	for _, ref := range r.EvidenceRefs {
		if _, err := uuid.Parse(ref.ID); err != nil || ref.Revision < 1 {
			return nil
		}
		switch ref.Kind {
		case "catalog_fact", "reviewed_usage_fact":
			hasFact = true
		case "tariff":
			hasTariff = true
		default:
			return nil
		}
	}
	if !hasFact || !hasTariff || len(r.EvidenceRefs) > 4 {
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
	err = tx.QueryRow(ctx, `SELECT version,timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&version, &timezone)
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
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return Preview{}, fmt.Errorf("invalid home timezone")
	}
	local := now.In(loc)
	start := fmt.Sprintf("%04d-%02d-01", local.Year(), local.Month())
	end := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc).Format("2006-01-02")
	rows, err := tx.Query(ctx, `SELECT a.id,a.category,a.version,e.id,e.result_json,e.as_of
		FROM appliances a LEFT JOIN LATERAL (
		 SELECT e.id,e.result_json,e.as_of FROM estimate_runs e
		 WHERE e.home_id=a.home_id AND e.appliance_id=a.id AND e.appliance_version=a.version
		   AND e.period_start=$3::date AND e.period_end=$4::date
		   AND NOT EXISTS(SELECT 1 FROM estimate_run_invalidations i WHERE i.run_id=e.id)
		 ORDER BY e.as_of DESC,e.id DESC LIMIT 1
		) e ON true
		WHERE a.home_id=$1 AND a.id=ANY($2::uuid[]) AND a.archived_at IS NULL ORDER BY a.id`, home, selected, start, end)
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{ID: uuid.NewString(), HomeID: home, SchemaVersion: "manual-preview/v1", CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(24 * time.Hour), Scope: "selected_appliances", HomeTotalStatus: "unavailable_incomplete_scope", MeasuredStatus: "not_included", RecommendationsStatus: "not_included", Assumptions: []string{"Per-device estimates use reviewed source evidence and a current monthly tariff.", "No whole-home total, measured savings, waste finding, or incentive is asserted."}, Items: []Item{}}
	type dependency struct {
		id         uuid.UUID
		version    int64
		estimateID *uuid.UUID
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
		item := Item{ApplianceID: id.String(), Category: safeCategory(category), Explanation: "No current reviewed monthly estimate is available for this appliance."}
		if runID != nil && asOf != nil {
			item.Estimate = vettedEstimate(body, start, end, *asOf)
			if item.Estimate != nil {
				item.Explanation = "Estimated monthly operating or useful energy cost; this is not measured use or a savings claim."
			} else {
				runID = nil
			}
		}
		preview.Items = append(preview.Items, item)
		deps = append(deps, dependency{id: id, version: applianceVersion, estimateID: runID})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return Preview{}, err
	}
	if len(preview.Items) != len(selected) {
		return Preview{}, ErrInvalidSelection
	}
	sort.Slice(preview.Items, func(i, j int) bool { return preview.Items[i].ApplianceID < preview.Items[j].ApplianceID })
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
		_, err = tx.Exec(ctx, `INSERT INTO report_preview_items(preview_id,appliance_id,appliance_version,estimate_run_id) VALUES($1,$2,$3,$4)`, preview.ID, d.id, d.version, d.estimateID)
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
	err = s.DB.QueryRow(ctx, `SELECT h.version<>$2 OR EXISTS (
		SELECT 1 FROM report_preview_items i LEFT JOIN appliances a ON a.id=i.appliance_id AND a.home_id=h.id
		WHERE i.preview_id=$1 AND (a.id IS NULL OR a.archived_at IS NOT NULL OR a.version<>i.appliance_version OR
		(i.estimate_run_id IS NOT NULL AND EXISTS(SELECT 1 FROM estimate_run_invalidations v WHERE v.run_id=i.estimate_run_id)))
	) FROM homes h WHERE h.id=$3 AND h.owner_user_id=$4 AND h.deleted_at IS NULL`, previewID, homeVersion, home, owner).Scan(&stale)
	if err != nil {
		return Preview{}, err
	}
	out.Stale = stale || !now.Before(expires)
	return out, nil
}
