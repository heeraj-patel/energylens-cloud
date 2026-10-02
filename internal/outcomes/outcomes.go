package outcomes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("idempotency key reused")

const window = 7 * 24 * time.Hour
const hours = 7 * 24

type Action struct {
	ID                string     `json:"id"`
	HomeID            string     `json:"home_id"`
	ApplianceID       string     `json:"appliance_id"`
	AttachmentID      string     `json:"attachment_id"`
	RuleID            string     `json:"rule_id"`
	OccurredAt        time.Time  `json:"occurred_at"`
	ComparableUse     bool       `json:"comparable_use"`
	ComparableWeather bool       `json:"comparable_weather"`
	Confounders       []string   `json:"confounders"`
	CreatedAt         time.Time  `json:"created_at"`
	SensorID          string     `json:"-"`
	HubID             string     `json:"-"`
	Epoch             int64      `json:"-"`
	AttachedFrom      time.Time  `json:"-"`
	AttachedUntil     *time.Time `json:"-"`
}

type Create struct {
	ApplianceID       string    `json:"appliance_id"`
	AttachmentID      string    `json:"attachment_id"`
	RuleID            string    `json:"rule_id"`
	OccurredAt        time.Time `json:"occurred_at"`
	ComparableUse     bool      `json:"comparable_use"`
	ComparableWeather bool      `json:"comparable_weather"`
	Confounders       []string  `json:"confounders"`
}

type Bucket struct {
	At         time.Time
	Wh         float64
	Covered    float64
	Status     string
	Allocation string
	GapCount   int
}

type Period struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	ExpectedHours    int       `json:"expected_hours"`
	MeasuredHours    int       `json:"measured_hours"`
	CoveredSeconds   float64   `json:"covered_seconds"`
	CoverageFraction float64   `json:"coverage_fraction"`
	KnownGapHours    int       `json:"known_gap_hours"`
	AllocatedHours   int       `json:"allocated_hours"`
	EnergyWh         *float64  `json:"energy_wh"`
}

type Outcome struct {
	Action         Action    `json:"action"`
	Status         string    `json:"status"`
	Reasons        []string  `json:"reasons"`
	Before         Period    `json:"before"`
	After          Period    `json:"after"`
	DeltaWh        *float64  `json:"delta_wh"`
	EvaluatedAt    time.Time `json:"evaluated_at"`
	Interpretation string    `json:"interpretation"`
}

type Store struct{ Pool *pgxpool.Pool }

// CreateAction serializes an owner-scoped replay key with the intervention.
func (s Store) CreateAction(ctx context.Context, owner, home, key string, in Create) (Action, error) {
	var a Action
	if in.Confounders == nil {
		in.Confounders = []string{}
	}
	encoded, _ := json.Marshal(in)
	hash := sha256.Sum256(encoded)
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	// Lock the home to prevent transfer during the action write.
	var ok string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	var oldHash []byte
	var oldID string
	err = tx.QueryRow(ctx, `SELECT request_hash,intervention_id FROM intervention_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND key=$3`, owner, home, key).Scan(&oldHash, &oldID)
	if err == nil {
		if string(oldHash) != string(hash[:]) {
			return a, ErrConflict
		}
		if err = getActionTx(ctx, tx, owner, home, oldID, &a); err != nil {
			return a, err
		}
		return a, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	var attachedAppliance string
	err = tx.QueryRow(ctx, `SELECT a.appliance_id FROM attachments a JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND a.appliance_id=$3 AND a.started_at<=$4 AND (a.ended_at IS NULL OR a.ended_at>$4) AND s.ownership_epoch>0`, in.AttachmentID, home, in.ApplianceID, in.OccurredAt).Scan(&attachedAppliance)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a = Action{ID: uuid.NewString(), HomeID: home, ApplianceID: in.ApplianceID, AttachmentID: in.AttachmentID, RuleID: in.RuleID, OccurredAt: in.OccurredAt, ComparableUse: in.ComparableUse, ComparableWeather: in.ComparableWeather, Confounders: in.Confounders}
	err = tx.QueryRow(ctx, `INSERT INTO interventions(id,home_id,appliance_id,attachment_id,rule_id,occurred_at,comparable_use,comparable_weather,confounders) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`, a.ID, home, a.ApplianceID, a.AttachmentID, a.RuleID, a.OccurredAt, a.ComparableUse, a.ComparableWeather, a.Confounders).Scan(&a.CreatedAt)
	if err != nil {
		return a, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO intervention_idempotency(owner_user_id,home_id,key,request_hash,intervention_id) VALUES($1,$2,$3,$4,$5)`, owner, home, key, hash[:], a.ID)
	if err != nil {
		return a, err
	}
	return a, tx.Commit(ctx)
}

func getActionTx(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner, home, id string, a *Action) error {
	err := q.QueryRow(ctx, `SELECT i.id,i.home_id,i.appliance_id,i.attachment_id,i.rule_id,i.occurred_at,i.comparable_use,i.comparable_weather,i.confounders,i.created_at,a.sensor_id,s.hub_id,s.ownership_epoch,a.started_at,a.ended_at FROM interventions i JOIN homes h ON h.id=i.home_id AND h.owner_user_id=$1 AND h.deleted_at IS NULL JOIN attachments a ON a.id=i.attachment_id AND a.home_id=i.home_id AND a.appliance_id=i.appliance_id JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id WHERE i.home_id=$2 AND i.id=$3`, owner, home, id).Scan(&a.ID, &a.HomeID, &a.ApplianceID, &a.AttachmentID, &a.RuleID, &a.OccurredAt, &a.ComparableUse, &a.ComparableWeather, &a.Confounders, &a.CreatedAt, &a.SensorID, &a.HubID, &a.Epoch, &a.AttachedFrom, &a.AttachedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s Store) GetAction(ctx context.Context, owner, home, id string) (Action, error) {
	var a Action
	err := getActionTx(ctx, s.Pool, owner, home, id, &a)
	return a, err
}

func (s Store) Buckets(ctx context.Context, owner, home string, a Action) ([]Bucket, error) {
	rows, err := s.Pool.Query(ctx, `SELECT e.bucket_start,e.energy_wh::float8,e.covered_seconds::float8,e.status,e.allocation_method,COALESCE(c.gap_overlap_count,0) FROM energy_hourly e JOIN homes h ON h.id=e.home_id AND h.owner_user_id=$1 AND h.deleted_at IS NULL LEFT JOIN coverage_hourly c ON c.home_id=e.home_id AND c.hub_id=e.hub_id AND c.ownership_epoch=e.ownership_epoch AND c.sensor_id=e.sensor_id AND c.bucket_start=e.bucket_start WHERE e.home_id=$2 AND e.attachment_id=$3 AND e.sensor_id=$4 AND e.hub_id=$5 AND e.ownership_epoch=$6 AND e.bucket_start >= $7 AND e.bucket_start < $8 ORDER BY e.bucket_start`, owner, home, a.AttachmentID, a.SensorID, a.HubID, a.Epoch, a.OccurredAt.Add(-window), a.OccurredAt.Add(window))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		var wh, covered *float64
		if err = rows.Scan(&b.At, &wh, &covered, &b.Status, &b.Allocation, &b.GapCount); err != nil {
			return nil, err
		}
		if wh != nil {
			b.Wh = *wh
		}
		if covered != nil {
			b.Covered = *covered
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Ready requires the rollup worker to have consumed all accepted batches and
// corrected buckets that could affect this attachment's comparison window.
func (s Store) Ready(ctx context.Context, owner, home string, a Action) (bool, error) {
	var ready bool
	err := s.Pool.QueryRow(ctx, `SELECT
	  (SELECT last_cursor FROM metered_rollup_checkpoint WHERE singleton=true) >=
	    COALESCE((SELECT max(b.cursor) FROM ingest_batches b WHERE b.home_id=$2),0)
	  AND NOT EXISTS (
	    SELECT 1 FROM metered_rollup_dirty d
	    WHERE d.home_id=$2 AND d.sensor_id=$3 AND (d.attachment_id=$4 OR d.attachment_id IS NULL)
	      AND d.bucket_start >= $5 AND d.bucket_start < $6
	  )
	  AND EXISTS (SELECT 1 FROM homes h WHERE h.id=$2 AND h.owner_user_id=$1 AND h.deleted_at IS NULL)`,
		owner, home, a.SensorID, a.AttachmentID, a.OccurredAt.Add(-window), a.OccurredAt.Add(window)).Scan(&ready)
	return ready, err
}

// Evaluate requires two complete, same-length UTC windows. Power samples and
// gaps never contribute energy. Self-reported comparability is context only.
func Evaluate(a Action, buckets []Bucket, now time.Time) Outcome {
	before := period(a.OccurredAt.Add(-window), a.OccurredAt, buckets)
	after := period(a.OccurredAt, a.OccurredAt.Add(window), buckets)
	o := Outcome{Action: a, Status: "collecting", Reasons: []string{}, Before: before, After: after, EvaluatedAt: now.UTC(), Interpretation: "No causal savings conclusion can be drawn from this uncontrolled before/after comparison."}
	if now.Before(after.To) {
		o.Reasons = append(o.Reasons, "follow_up_window_open")
		return o
	}
	o.Status = "inconclusive"
	if a.AttachedFrom.After(before.From) || a.AttachedUntil != nil && a.AttachedUntil.Before(after.To) {
		o.Reasons = append(o.Reasons, "attachment_changed")
	}
	if !a.ComparableUse {
		o.Reasons = append(o.Reasons, "use_not_confirmed_comparable")
	}
	if !a.ComparableWeather {
		o.Reasons = append(o.Reasons, "weather_not_confirmed_comparable")
	}
	if len(a.Confounders) > 0 {
		o.Reasons = append(o.Reasons, "reported_confounders")
	}
	if !adequate(before) || !adequate(after) {
		o.Reasons = append(o.Reasons, "insufficient_measured_coverage")
	}
	if before.AllocatedHours > 0 || after.AllocatedHours > 0 {
		o.Reasons = append(o.Reasons, "allocated_measurement")
	}
	if len(o.Reasons) > 0 {
		return o
	}
	// An observed difference below 5% of baseline is reported as inconclusive.
	delta := *after.EnergyWh - *before.EnergyWh
	if *before.EnergyWh <= 0 || math.Abs(delta) < 0.05**before.EnergyWh {
		o.Reasons = append(o.Reasons, "no_clear_change")
		return o
	}
	o.Status = "observed-change"
	o.DeltaWh = &delta
	return o
}

func period(from, to time.Time, rows []Bucket) Period {
	p := Period{From: from, To: to, ExpectedHours: hours}
	seen := map[time.Time]bool{}
	var energy float64
	for _, b := range rows {
		if b.At.Before(from) || !b.At.Before(to) || seen[b.At] {
			continue
		}
		seen[b.At] = true
		if b.Status != "complete" || b.Covered < 3240 || b.Covered > 3600 || math.IsNaN(b.Wh) || math.IsInf(b.Wh, 0) || b.Wh < 0 {
			continue
		}
		p.MeasuredHours++
		p.CoveredSeconds += b.Covered
		energy += b.Wh
		if b.GapCount > 0 {
			p.KnownGapHours++
		}
		if b.Allocation != "exact" {
			p.AllocatedHours++
		}
	}
	p.CoverageFraction = p.CoveredSeconds / (hours * 3600)
	if p.MeasuredHours == hours && p.KnownGapHours == 0 && p.CoverageFraction >= 0.95 {
		p.EnergyWh = &energy
	}
	return p
}

func adequate(p Period) bool {
	return p.EnergyWh != nil && p.MeasuredHours == hours && p.KnownGapHours == 0 && p.CoverageFraction >= 0.95
}

var AllowedConfounders = map[string]bool{"occupancy_changed": true, "use_changed": true, "weather_changed": true, "equipment_changed": true, "other": true, "unknown": true}

func ValidCreate(in Create, now time.Time) bool {
	if uuid.Validate(in.ApplianceID) != nil || uuid.Validate(in.AttachmentID) != nil || len(in.RuleID) < 1 || len(in.RuleID) > 100 || in.OccurredAt.IsZero() || in.OccurredAt.Location() != time.UTC || !in.OccurredAt.Equal(in.OccurredAt.Truncate(time.Hour)) || in.OccurredAt.After(now) || in.OccurredAt.Before(now.Add(-23*24*time.Hour)) || len(in.Confounders) > 8 {
		return false
	}
	for _, c := range in.Confounders {
		if !AllowedConfounders[c] {
			return false
		}
	}
	sort.Strings(in.Confounders)
	return true
}
