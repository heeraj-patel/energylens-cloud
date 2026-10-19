package householdenergy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	Pool   *pgxpool.Pool
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
	Now    func() time.Time
}

type quantity struct {
	Value string `json:"value"`
	Unit  string `json:"unit"`
}
type allocation struct {
	Energy             *quantity `json:"energy"`
	Cost               any       `json:"cost"`
	UsefulEnergy       any       `json:"useful_energy"`
	UsefulCost         any       `json:"useful_cost"`
	WasteEnergy        any       `json:"waste_energy"`
	WasteCost          any       `json:"waste_cost"`
	UnclassifiedEnergy *quantity `json:"unclassified_energy"`
	UnclassifiedCost   any       `json:"unclassified_cost"`
	Availability       string    `json:"availability"`
	EvidenceBasis      string    `json:"evidence_basis"`
}
type money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}
type period struct {
	Start time.Time `json:"start_local"`
	End   time.Time `json:"end_exclusive_local"`
}
type base struct {
	SchemaRevision  string     `json:"schema_revision"`
	ContentRevision string     `json:"content_revision"`
	HomeID          string     `json:"home_id"`
	DataMode        string     `json:"data_mode"`
	SnapshotID      string     `json:"snapshot_id"`
	GeneratedAt     time.Time  `json:"generated_at"`
	AsOf            *time.Time `json:"as_of"`
	Timezone        string     `json:"timezone"`
	Period          period     `json:"period"`
	Scope           string     `json:"scope"`
	Availability    string     `json:"availability"`
	EvidenceBasis   string     `json:"evidence_basis"`
	FixtureRevision string     `json:"fixture_revision,omitempty"`
}
type bucket struct {
	Start             time.Time          `json:"start_local"`
	End               time.Time          `json:"end_exclusive_local"`
	Allocation        allocation         `json:"allocation"`
	CoverageFraction  any                `json:"coverage_fraction"`
	DeviceAllocations []deviceAllocation `json:"device_allocations,omitempty"`
	Unattributed      *allocation        `json:"unattributed,omitempty"`
	energyKWh         *big.Rat           `json:"-"`
	selected          []interval         `json:"-"`
	deviceItems       []interval         `json:"-"`
	wholeHome         bool               `json:"-"`
}
type deviceAllocation struct {
	ApplianceID string     `json:"appliance_id,omitempty"`
	DeviceID    string     `json:"device_id,omitempty"`
	Name        *string    `json:"name,omitempty"`
	Allocation  allocation `json:"allocation"`
}

func (d deviceAllocation) MarshalJSON() ([]byte, error) {
	if d.DeviceID != "" {
		return json.Marshal(struct {
			DeviceID   string     `json:"device_id"`
			Name       *string    `json:"name"`
			Allocation allocation `json:"allocation"`
		}{d.DeviceID, d.Name, d.Allocation})
	}
	return json.Marshal(struct {
		ApplianceID string     `json:"appliance_id"`
		Allocation  allocation `json:"allocation"`
	}{d.ApplianceID, d.Allocation})
}

type interval struct {
	Start, End                             time.Time
	Wh                                     *big.Rat
	Fuel                                   *string
	ApplianceID                            string
	SensorKind                             string
	ParentSensorID                         *string
	TopologyRootID, TopologyRootKind       *string
	TopologyComplete, TopologyTerminal     bool
	HubID, SensorID, AttachmentID, EventID string
	Epoch, Revision                        int64
	Unavailable                            bool
	Summarized                             bool
	Covered                                *big.Rat
}
type marginalTariff struct {
	ID, Currency string
	Revision     int64
	Rate         *big.Rat
	From         time.Time
	To           *time.Time
	SourceAsOf   time.Time
	Segments     []marginalTariff
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/dashboard", s.dashboard)
	r.With(s.Auth).Get("/homes/{home}/energy-trends", s.trends)
}

func (s Service) dashboard(w http.ResponseWriter, r *http.Request) {
	s.read(w, r, "dashboard", "hour", "")
}
func (s Service) trends(w http.ResponseWriter, r *http.Request) {
	g, window, ok := parseTrendsQuery(r.URL.Query())
	if !ok {
		fail(w, 400, "invalid_request")
		return
	}
	s.read(w, r, "energy_trends", g, window)
}

func parseTrendsQuery(query url.Values) (granularity, window string, ok bool) {
	values := query["granularity"]
	if len(values) != 1 {
		return "", "", false
	}
	granularity = values[0]
	if granularity != "hour" && granularity != "day" && granularity != "week" && granularity != "month" {
		return "", "", false
	}
	if len(query) == 1 {
		return granularity, "", true
	}
	windows := query["window"]
	if len(query) == 2 && len(windows) == 1 {
		if windows[0] == "month_to_date" && granularity == "day" {
			return granularity, windows[0], true
		}
		if windows[0] == "trailing_30_days" && granularity == "day" {
			return granularity, windows[0], true
		}
		if windows[0] == "last_5_occurrences" && (granularity == "day" || granularity == "week" || granularity == "month") {
			return granularity, windows[0], true
		}
	}
	return "", "", false
}

func (s Service) read(w http.ResponseWriter, r *http.Request, view, granularity, window string) {
	w.Header().Set("Cache-Control", "private, no-store")
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, 404, "not_found")
		return
	}
	if view == "dashboard" && r.URL.RawQuery != "" {
		fail(w, 400, "invalid_request")
		return
	}
	tx, err := s.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var tz string
	var homeVersion int64
	var rawDemo []byte
	err = tx.QueryRow(r.Context(), `SELECT timezone,version,demo_cost_snapshot FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, s.UserID(r)).Scan(&tz, &homeVersion, &rawDemo)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		internal(w, err)
		return
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	p := periodFor(now, loc, granularity)
	if window == "month_to_date" {
		p = monthToDatePeriod(now, loc)
	} else if window == "trailing_30_days" {
		p = trailing30DaysPeriod(now, loc)
	} else if window == "last_5_occurrences" {
		p = last5OccurrencesPeriod(now, loc, granularity)
	}
	var watermark int64
	err = tx.QueryRow(r.Context(), `SELECT COALESCE(max(cursor),0) FROM ingest_batches WHERE home_id=$1`, home).Scan(&watermark)
	if err != nil {
		internal(w, err)
		return
	}
	var hasHistory, hasInventory bool
	err = tx.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM ingest_events WHERE home_id=$1 AND event_kind='energy_interval'),
 EXISTS(SELECT 1 FROM appliances WHERE home_id=$1)`, home).Scan(&hasHistory, &hasInventory)
	if err != nil {
		internal(w, err)
		return
	}
	items, _, err := readIntervals(r.Context(), tx, home, p, now)
	if err != nil {
		internal(w, err)
		return
	}
	items, err = mergeRetainedIntervals(r.Context(), tx, home, p, items)
	if err != nil {
		internal(w, err)
		return
	}
	var summaryRevision int64
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE(revision,0) FROM metered_summary_revision WHERE home_id=$1`, home).Scan(&summaryRevision); errors.Is(err, pgx.ErrNoRows) {
		summaryRevision = 0
	} else if err != nil {
		internal(w, err)
		return
	}
	buckets := makeBuckets(p, granularity, items)
	selectedItems := make([]interval, 0, len(items))
	for _, row := range buckets {
		selectedItems = append(selectedItems, row.selected...)
	}
	latestTariffID, latestTariffRevision, tariff, err := readMarginalTariff(r.Context(), tx, home, now, selectedItems)
	if err != nil {
		internal(w, err)
		return
	}
	var demoSnapshot *DemoCostSnapshot
	if len(rawDemo) > 0 && string(rawDemo) != "null" {
		var parsed DemoCostSnapshot
		if json.Unmarshal(rawDemo, &parsed) == nil && parsed.Validate() == nil && parsed.Timezone == tz {
			demoSnapshot = &parsed
		}
	}
	sample := demoSnapshot != nil && !hasHistory && !hasInventory
	seededTotals := demoSnapshot != nil && !hasHistory
	mixed := demoSnapshot != nil && (hasHistory || hasInventory)
	var asOf *time.Time
	if demoSnapshot != nil && !hasHistory {
		asOf = &now
	} else if hasHistory {
		asOf, err = homeEnergyAsOf(r.Context(), tx, home, now)
		if err != nil {
			internal(w, err)
			return
		}
	}
	var sampleUnattributed *allocation
	var sampleDeviceRows []deviceAllocation
	var demoBuckets []bucket
	if demoSnapshot != nil {
		var projectedTotal allocation
		demoBuckets, projectedTotal, sampleDeviceRows, sampleUnattributed = projectDemoSnapshot(*demoSnapshot, p, granularity, now)
		_ = projectedTotal
		if seededTotals {
			buckets = demoBuckets
		} else {
			// Keep seeded per-device costs available beside measured home totals.
			// They are separate allocations and must not be added to those totals.
			for i := range buckets {
				if i < len(demoBuckets) {
					buckets[i].DeviceAllocations = demoBuckets[i].DeviceAllocations
				}
			}
			sampleUnattributed = nil
		}
	}
	var total *big.Rat
	for _, b := range buckets {
		if b.energyKWh != nil {
			if total == nil {
				total = new(big.Rat)
			}
			total.Add(total, b.energyKWh)
		}
	}
	// Daily/Weekly/Monthly Trends must be comparable at a glance: each bucket
	// here is one full day/week/month, so the period total is averaged over
	// the elapsed buckets that have data rather than shown as a running sum.
	averaged := window == "last_5_occurrences"
	periodsWithData := countPeriodsWithData(buckets)
	if averaged {
		total = averageOverPeriods(total, periodsWithData)
	}
	a := fromKWh(total)
	if seededTotals {
		// The cost-only snapshot intentionally provides no kWh or tariff.
		a = sumCostAllocations(sampleDeviceRows, func() allocation {
			if sampleUnattributed != nil {
				return *sampleUnattributed
			}
			return allocation{}
		}())
	}
	if !sample && tariff != nil {
		if cost := priceEffectiveBuckets(buckets, selectedItems, *tariff); cost != nil {
			if averaged {
				*cost = averageOverMoney(*cost, periodsWithData)
			}
			a.Cost = *cost
			a.UnclassifiedCost = *cost
			a.EvidenceBasis = "mixed"
		}
	}
	// Real appliance rows are derived from the same observations as the home
	// total. A bucket with overlapping observations has no safe allocation.
	if !seededTotals {
		realRows, deviceWh := attributeDeviceBuckets(buckets, items, nil)
		periodRows := sumDeviceAllocations(deviceWh, nil)
		if tariff != nil {
			priceAttributedDevices(buckets, realRows, periodRows, selectedItems, *tariff)
		}
		for i := range buckets {
			buckets[i].DeviceAllocations = append(buckets[i].DeviceAllocations, realRows[i]...)
		}
		sampleDeviceRows = append(sampleDeviceRows, periodRows...)
		if len(buckets) > 0 && allWholeHomeBuckets(buckets) {
			if residual := measuredResidual(a, periodRows); residual != nil {
				sampleUnattributed = residual
			}
			for i := range buckets {
				buckets[i].Unattributed = measuredResidual(buckets[i].Allocation, realRows[i])
			}
		}
	}
	resolvedBuckets, resolvedPeriod, err := resolutionDeviceRows(r.Context(), tx, home, buckets, now)
	if err != nil {
		internal(w, err)
		return
	}
	for i := range buckets {
		buckets[i].DeviceAllocations = append(buckets[i].DeviceAllocations, resolvedBuckets[i]...)
	}
	sampleDeviceRows = append(sampleDeviceRows, resolvedPeriod...)
	b := base{SchemaRevision: "household_energy_v1", ContentRevision: "household_energy_backend_v1", HomeID: home, DataMode: "household", GeneratedAt: now, AsOf: asOf, Timezone: tz, Period: p, Scope: "attributed_devices", Availability: a.Availability, EvidenceBasis: a.EvidenceBasis}
	if sample {
		b.DataMode = "sample"
		b.ContentRevision = demoSnapshot.FixtureRevision
		b.FixtureRevision = demoSnapshot.FixtureRevision
		b.Scope = "whole_home_including_unattributed_load"
	}
	if mixed {
		b.DataMode = "mixed"
		b.FixtureRevision = demoSnapshot.FixtureRevision
		if seededTotals {
			b.ContentRevision = demoSnapshot.FixtureRevision
			b.Scope = "whole_home_including_unattributed_load"
		}
	}
	if !seededTotals && allWholeHomeBuckets(buckets) {
		b.Scope = "whole_home_including_unattributed_load"
	}
	var efficiency *efficiencyMonthToDate
	if view == "energy_trends" && window == "month_to_date" && seededTotals {
		efficiency = syntheticEfficiencyMonthToDate(a)
	}
	// Tariff eligibility can change as time passes, without a new tariff
	// revision (for example, at source_as_of + 1440 hours). Include the
	// customer-visible allocation in the identity so a newly priced or newly
	// unpriced response cannot reuse the prior snapshot ID.
	allocationState := allocationSnapshotState(a)
	if efficiency != nil {
		state, _ := json.Marshal(efficiency)
		allocationState += "|" + string(state)
	}
	deviceState, _ := json.Marshal(sampleDeviceRows)
	allocationState += "|" + string(deviceState)
	for _, row := range buckets {
		deviceState, _ := json.Marshal(row.DeviceAllocations)
		allocationState += "|" + string(deviceState)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d|%d|%d|%s|%s|%s|%d|%s", home, tz, p.Start.UTC().Format(time.RFC3339Nano), p.End.UTC().Format(time.RFC3339Nano), homeVersion, watermark, summaryRevision, granularity, b.DataMode, latestTariffID, latestTariffRevision, allocationState)))
	b.SnapshotID = hex.EncodeToString(digest[:])
	if err = tx.Commit(r.Context()); err != nil {
		internal(w, err)
		return
	}
	var out any
	if view == "dashboard" {
		out = struct {
			base
			View                string             `json:"view"`
			Totals              allocation         `json:"totals"`
			DeviceAllocations   []deviceAllocation `json:"device_allocations,omitempty"`
			Unattributed        *allocation        `json:"unattributed,omitempty"`
			TrendPeriod         string             `json:"trend_period"`
			TopRecommendationID any                `json:"top_recommendation_id"`
			AssistantIntentID   any                `json:"assistant_intent_id"`
		}{b, "dashboard", a, sampleDeviceRows, sampleUnattributed, "day", nil, nil}
	} else {
		out = struct {
			base
			View         string                 `json:"view"`
			Granularity  string                 `json:"granularity"`
			Totals       allocation             `json:"totals"`
			Buckets      []bucket               `json:"buckets"`
			Unattributed *allocation            `json:"unattributed,omitempty"`
			Efficiency   *efficiencyMonthToDate `json:"efficiency_month_to_date,omitempty"`
		}{b, "energy_trends", granularity, a, buckets, sampleUnattributed, efficiency}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func allocationSnapshotState(a allocation) string {
	state, _ := json.Marshal(a)
	return string(state)
}

func periodFor(now time.Time, loc *time.Location, g string) period {
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch g {
	case "day":
		start := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		return period{start, start.AddDate(0, 0, 7)}
	case "week":
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		return period{start, start.AddDate(0, 1, 0)}
	case "month":
		start := time.Date(local.Year(), 1, 1, 0, 0, 0, 0, loc)
		return period{start, start.AddDate(1, 0, 0)}
	default:
		return period{day, day.AddDate(0, 0, 1)}
	}
}

func trailing30DaysPeriod(now time.Time, loc *time.Location) period {
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return period{Start: today.AddDate(0, 0, -29), End: today.AddDate(0, 0, 1)}
}

func monthToDatePeriod(now time.Time, loc *time.Location) period {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return period{Start: start, End: today.AddDate(0, 0, 1)}
}

// last5OccurrencesPeriod returns the five home-local calendar buckets ending
// with the current occurrence. Weeks start Monday; day/week/month boundaries
// are local-calendar boundaries, so DST changes do not shift bucket dates.
func last5OccurrencesPeriod(now time.Time, loc *time.Location, granularity string) period {
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch granularity {
	case "day":
		return period{Start: day.AddDate(0, 0, -4), End: day.AddDate(0, 0, 1)}
	case "week":
		weekStart := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		return period{Start: weekStart.AddDate(0, 0, -28), End: weekStart.AddDate(0, 0, 7)}
	case "month":
		monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		return period{Start: monthStart.AddDate(0, -4, 0), End: monthStart.AddDate(0, 1, 0)}
	default:
		return period{}
	}
}

func readIntervals(ctx context.Context, tx pgx.Tx, home string, p period, now time.Time) ([]interval, *time.Time, error) {
	rows, err := tx.Query(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id) e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.event_kind
 FROM ingest_events e WHERE e.home_id=$1 AND e.event_kind='energy_interval' ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC)
		SELECT o.start_at,o.end_at,o.energy_wh::numeric::text,o.fuel,a.appliance_id::text,s.kind,s.parent_sensor_id::text,o.hub_id::text,e.sensor_id::text,o.attachment_revision_id::text,o.event_id::text,o.ownership_epoch,o.revision,o.covered_seconds,topology.root_id::text,topology.root_kind,topology.root_id IS NOT NULL,NOT EXISTS (SELECT 1 FROM sensor_endpoints child WHERE child.home_id=s.home_id AND child.hub_id=s.hub_id AND child.ownership_epoch=s.ownership_epoch AND child.parent_sensor_id=s.id) FROM latest l
 JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision
	 JOIN ingest_events e ON e.hub_id=o.hub_id AND e.ownership_epoch=o.ownership_epoch AND e.event_id=o.event_id AND e.revision=o.revision AND e.home_id=$1
 JOIN attachments a ON a.id=o.attachment_revision_id AND a.home_id=$1
	 JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.id=e.sensor_id AND s.home_id=$1 AND s.hub_id=o.hub_id AND s.ownership_epoch=o.ownership_epoch
	 LEFT JOIN LATERAL (WITH RECURSIVE ancestry(id,parent_sensor_id,kind,depth) AS (
	   SELECT s.id,s.parent_sensor_id,s.kind,0
	   UNION ALL
	   SELECT parent.id,parent.parent_sensor_id,parent.kind,ancestry.depth+1
	   FROM ancestry JOIN sensor_endpoints parent ON parent.id=ancestry.parent_sensor_id AND parent.home_id=s.home_id AND parent.hub_id=s.hub_id AND parent.ownership_epoch=s.ownership_epoch
	   WHERE ancestry.depth < 32
	 ) SELECT id AS root_id,kind AS root_kind FROM ancestry WHERE parent_sensor_id IS NULL ORDER BY depth DESC LIMIT 1) topology ON TRUE
		 WHERE l.event_kind='energy_interval' AND o.start_at < $3 AND o.end_at > $2 AND o.end_at <= $4 AND o.end_at > o.start_at ORDER BY o.start_at,o.event_id`, home, p.Start, p.End, now)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := []interval{}
	var asOf *time.Time
	for rows.Next() {
		var v interval
		var raw string
		var fuel *string
		var covered int64
		if err = rows.Scan(&v.Start, &v.End, &raw, &fuel, &v.ApplianceID, &v.SensorKind, &v.ParentSensorID, &v.HubID, &v.SensorID, &v.AttachmentID, &v.EventID, &v.Epoch, &v.Revision, &covered, &v.TopologyRootID, &v.TopologyRootKind, &v.TopologyComplete, &v.TopologyTerminal); err != nil {
			return nil, nil, err
		}
		v.Fuel = fuel
		v.Covered = new(big.Rat).SetInt64(covered)
		var ok bool
		v.Wh, ok = new(big.Rat).SetString(raw)
		if !ok {
			return nil, nil, fmt.Errorf("invalid persisted energy decimal")
		}
		// Keep only the part of the observation represented by this response.
		// Counter intervals are assumed to distribute energy uniformly over
		// their duration, matching meteredrollup's boundary allocation rule.
		if !clipInterval(&v, p) {
			continue
		}
		items = append(items, v)
		if asOf == nil || v.End.After(*asOf) {
			t := v.End
			asOf = &t
		}
	}
	return items, asOf, rows.Err()
}

func clipInterval(v *interval, p period) bool {
	start, end := maxTime(v.Start, p.Start), minTime(v.End, p.End)
	if !end.After(start) {
		return false
	}
	fraction := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(v.End.Sub(v.Start).Nanoseconds()))
	v.Wh = new(big.Rat).Mul(v.Wh, fraction)
	if v.Covered != nil {
		v.Covered = new(big.Rat).Mul(v.Covered, fraction)
	}
	v.Start, v.End = start, end
	return true
}

// homeEnergyAsOf is independent of the requested chart window. All views of
// one home therefore report the same live evidence cutoff, including a window
// that happens to contain no observations.
func homeEnergyAsOf(ctx context.Context, tx pgx.Tx, home string, now time.Time) (*time.Time, error) {
	var asOf *time.Time
	err := tx.QueryRow(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id) e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.event_kind
 FROM ingest_events e WHERE e.home_id=$1 AND e.event_kind='energy_interval'
 ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC)
 SELECT GREATEST(max(o.end_at), (SELECT max(h.bucket_start + interval '1 hour') FROM energy_hourly h WHERE h.home_id=$1 AND h.status='complete' AND h.bucket_start + interval '1 hour'<=$2)) FROM latest l
 JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision
 JOIN attachments a ON a.id=o.attachment_revision_id AND a.home_id=$1
 WHERE l.event_kind='energy_interval' AND o.end_at <= $2 AND o.end_at > o.start_at`, home, now).Scan(&asOf)
	if asOf != nil {
		utc := asOf.UTC()
		asOf = &utc
	}
	return asOf, err
}

func readMarginalTariff(ctx context.Context, tx pgx.Tx, home string, now time.Time, items []interval) (string, int64, *marginalTariff, error) {
	return readEffectiveTariff(ctx, tx, home, now, items)
}

func priceBuckets(buckets []bucket, t marginalTariff) *money {
	total := new(big.Rat)
	displayed := new(big.Rat)
	priced := false
	for i := range buckets {
		if buckets[i].energyKWh == nil {
			continue
		}
		amount := new(big.Rat).Mul(buckets[i].energyKWh, t.Rate)
		total.Add(total, amount)
		cumulative := mustDecimal(total.FloatString(6))
		shown := new(big.Rat).Sub(cumulative, displayed)
		// Preserve sub-cent costs in buckets and totals. Rounding each bucket
		// to currency cents makes the period total depend on chart granularity.
		buckets[i].Allocation.Cost = money{Amount: shown.FloatString(6), Currency: t.Currency}
		buckets[i].Allocation.UnclassifiedCost = money{Amount: shown.FloatString(6), Currency: t.Currency}
		buckets[i].Allocation.EvidenceBasis = "mixed"
		displayed.Set(cumulative)
		priced = true
	}
	if !priced {
		return nil
	}
	return &money{Amount: total.FloatString(6), Currency: t.Currency}
}

func mustDecimal(value string) *big.Rat {
	r, _ := new(big.Rat).SetString(value)
	return r
}

// Device allocations use the same topology decision as the household bucket.
func attributeDeviceBuckets(buckets []bucket, items []interval, tariff *marginalTariff) ([][]deviceAllocation, map[string]*big.Rat) {
	rows := make([][]deviceAllocation, len(buckets))
	periodWh := make(map[string]*big.Rat)
	for bi, b := range buckets {
		if b.Allocation.Energy == nil {
			continue
		}
		selected := b.deviceItems
		if selected == nil {
			_, selected, _ = selectBucketSources(items, b.Start, b.End)
		}
		bucketWh := make(map[string]*big.Rat)
		for _, item := range selected {
			if item.Unavailable || !terminalDevice(item) {
				continue
			}
			start, end := maxTime(item.Start, b.Start), minTime(item.End, b.End)
			share := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
			wh := new(big.Rat).Mul(item.Wh, share)
			if bucketWh[item.ApplianceID] == nil {
				bucketWh[item.ApplianceID] = new(big.Rat)
			}
			bucketWh[item.ApplianceID].Add(bucketWh[item.ApplianceID], wh)
		}
		ids := make([]string, 0, len(bucketWh))
		for id := range bucketWh {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rows[bi] = append(rows[bi], deviceAllocation{ApplianceID: id, Allocation: measuredDeviceAllocation(bucketWh[id], tariff)})
			if periodWh[id] == nil {
				periodWh[id] = new(big.Rat)
			}
			periodWh[id].Add(periodWh[id], bucketWh[id])
		}
	}
	return rows, periodWh
}

func sumDeviceAllocations(wh map[string]*big.Rat, tariff *marginalTariff) []deviceAllocation {
	ids := make([]string, 0, len(wh))
	for id := range wh {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]deviceAllocation, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, deviceAllocation{ApplianceID: id, Allocation: measuredDeviceAllocation(wh[id], tariff)})
	}
	return rows
}

func measuredDeviceAllocation(wh *big.Rat, tariff *marginalTariff) allocation {
	kwh := new(big.Rat).Quo(wh, big.NewRat(1000, 1))
	a := fromKWh(kwh)
	if tariff != nil {
		cost := money{Amount: new(big.Rat).Mul(kwh, tariff.Rate).FloatString(6), Currency: tariff.Currency}
		a.Cost, a.UnclassifiedCost, a.EvidenceBasis = cost, cost, "mixed"
	}
	return a
}

func makeBuckets(p period, g string, items []interval) []bucket {
	out := []bucket{}
	for start := p.Start; start.Before(p.End); {
		var end time.Time
		switch g {
		case "hour":
			end = start.Add(time.Hour)
		case "day":
			end = start.AddDate(0, 0, 1)
		case "week":
			end = start.AddDate(0, 0, 7)
		default:
			end = start.AddDate(0, 1, 0)
		}
		if end.After(p.End) {
			end = p.End
		}
		selected, devices, wholeHome := selectBucketSources(items, start, end)
		var wh *big.Rat
		for _, v := range selected {
			low, high := v.Start, v.End
			if low.Before(start) {
				low = start
			}
			if high.After(end) {
				high = end
			}
			if high.After(low) {
				if v.Unavailable || v.Summarized && (v.Start.Before(start) || v.End.After(end)) {
					continue
				}
				if wh == nil {
					wh = new(big.Rat)
				}
				share := new(big.Rat).SetFrac(big.NewInt(high.Sub(low).Nanoseconds()), big.NewInt(v.End.Sub(v.Start).Nanoseconds()))
				allocatedWh := new(big.Rat).Mul(v.Wh, share)
				wh.Add(wh, allocatedWh)
			}
		}
		for _, v := range selected {
			if v.Unavailable || v.Summarized && (v.Start.Before(start) || v.End.After(end)) {
				wh = nil
			}
		}
		var kwh *big.Rat
		if wh != nil {
			kwh = new(big.Rat).Quo(wh, big.NewRat(1000, 1))
		}
		coverage := any(nil)
		if wholeHome {
			coverage = selectedCoverage(selected, start, end)
		}
		out = append(out, bucket{Start: start, End: end, Allocation: fromKWh(kwh), CoverageFraction: coverage, energyKWh: kwh, selected: selected, deviceItems: devices, wholeHome: wholeHome})
		start = end
	}
	// Round cumulative energy so every nonnegative bucket remains nonnegative
	// while serialized buckets conserve the displayed period total.
	var raw, displayed big.Rat
	for i := range out {
		if out[i].energyKWh == nil {
			continue
		}
		raw.Add(&raw, out[i].energyKWh)
		cumulative := mustDecimal(raw.FloatString(12))
		value := new(big.Rat).Sub(cumulative, &displayed).FloatString(12)
		out[i].Allocation.Energy.Value = value
		out[i].Allocation.UnclassifiedEnergy.Value = value
		displayed.Set(cumulative)
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// countPeriodsWithData returns how many buckets have a measured energy
// value. For the last_5_occurrences window, each bucket is exactly one
// day/week/month, so this count is the number of elapsed days/weeks/months
// a period total should be divided by to show a comparable average.
func countPeriodsWithData(buckets []bucket) int {
	count := 0
	for _, b := range buckets {
		if b.energyKWh != nil {
			count++
		}
	}
	return count
}

func averageOverPeriods(total *big.Rat, periods int) *big.Rat {
	if total == nil || periods <= 0 {
		return total
	}
	return new(big.Rat).Quo(total, big.NewRat(int64(periods), 1))
}

func averageOverMoney(m money, periods int) money {
	if periods <= 0 {
		return m
	}
	amount, ok := new(big.Rat).SetString(m.Amount)
	if !ok {
		return m
	}
	amount.Quo(amount, big.NewRat(int64(periods), 1))
	m.Amount = amount.FloatString(6)
	return m
}

func fromKWh(v *big.Rat) allocation {
	a := allocation{Availability: "unavailable", EvidenceBasis: "unavailable"}
	if v != nil {
		q := &quantity{Value: v.FloatString(12), Unit: "kWh"}
		a.Energy = q
		a.UnclassifiedEnergy = q
		a.Availability = "partial"
		a.EvidenceBasis = "measured"
	}
	return a
}
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "", "request_id": uuid.NewString(), "details": map[string]any{}})
}
func internal(w http.ResponseWriter, err error) {
	slog.Error("household energy read failed", "error", err)
	fail(w, 500, "internal_error")
}
