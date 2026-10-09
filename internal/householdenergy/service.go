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
	Start, End time.Time
	Wh         *big.Rat
	Fuel       *string
}
type marginalTariff struct {
	ID, Currency string
	Revision     int64
	Rate         *big.Rat
	From         time.Time
	To           *time.Time
	SourceAsOf   time.Time
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
	if window == "trailing_30_days" {
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
	buckets := makeBuckets(p, granularity, items)
	latestTariffID, latestTariffRevision, tariff, err := readMarginalTariff(r.Context(), tx, home, now, items)
	if err != nil {
		internal(w, err)
		return
	}
	var demoSnapshot *DemoCostSnapshot
	if !hasHistory && !hasInventory && len(rawDemo) > 0 && string(rawDemo) != "null" {
		var parsed DemoCostSnapshot
		if err = json.Unmarshal(rawDemo, &parsed); err != nil || parsed.Validate() != nil || parsed.Timezone != tz {
			internal(w, fmt.Errorf("invalid stored demo cost snapshot"))
			return
		}
		demoSnapshot = &parsed
	}
	sample := demoSnapshot != nil
	var asOf *time.Time
	if sample {
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
	if sample {
		var projectedTotal allocation
		buckets, projectedTotal, sampleDeviceRows, sampleUnattributed = projectDemoSnapshot(*demoSnapshot, p, granularity, now)
		_ = projectedTotal
	}
	var total *big.Rat
	for _, b := range buckets {
		if b.Allocation.Energy != nil {
			v, _ := new(big.Rat).SetString(b.Allocation.Energy.Value)
			if total == nil {
				total = new(big.Rat)
			}
			total.Add(total, v)
		}
	}
	a := fromKWh(total)
	if sample {
		// The cost-only snapshot intentionally provides no kWh or tariff.
		a = sumCostAllocations(sampleDeviceRows, func() allocation {
			if sampleUnattributed != nil {
				return *sampleUnattributed
			}
			return allocation{}
		}())
	}
	if !sample && tariff != nil {
		if cost := priceBuckets(buckets, *tariff); cost != nil {
			a.Cost = *cost
			a.UnclassifiedCost = *cost
			a.EvidenceBasis = "mixed"
		}
	}
	b := base{SchemaRevision: "household_energy_v1", ContentRevision: "household_energy_backend_v1", HomeID: home, DataMode: "household", GeneratedAt: now, AsOf: asOf, Timezone: tz, Period: p, Scope: "attributed_devices", Availability: a.Availability, EvidenceBasis: a.EvidenceBasis}
	if sample {
		b.DataMode = "sample"
		b.ContentRevision = demoSnapshot.FixtureRevision
		b.FixtureRevision = demoSnapshot.FixtureRevision
		b.Scope = "whole_home_including_unattributed_load"
	}
	// Tariff eligibility can change as time passes, without a new tariff
	// revision (for example, at source_as_of + 1440 hours). Include the
	// customer-visible allocation in the identity so a newly priced or newly
	// unpriced response cannot reuse the prior snapshot ID.
	allocationState := allocationSnapshotState(a)
	if sample {
		deviceState, _ := json.Marshal(sampleDeviceRows)
		allocationState += "|" + string(deviceState)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s|%s|%s|%d|%s", home, tz, p.Start.UTC().Format(time.RFC3339Nano), p.End.UTC().Format(time.RFC3339Nano), homeVersion, watermark, granularity, b.DataMode, latestTariffID, latestTariffRevision, allocationState)))
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
			View         string      `json:"view"`
			Granularity  string      `json:"granularity"`
			Totals       allocation  `json:"totals"`
			Buckets      []bucket    `json:"buckets"`
			Unattributed *allocation `json:"unattributed,omitempty"`
		}{b, "energy_trends", granularity, a, buckets, sampleUnattributed}
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
		SELECT o.start_at,o.end_at,o.energy_wh::numeric::text,o.fuel FROM latest l
 JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision
 JOIN attachments a ON a.id=o.attachment_revision_id AND a.home_id=$1
	 WHERE l.event_kind='energy_interval' AND o.start_at >= $2 AND o.end_at <= $3 AND o.end_at <= $4 AND o.end_at > o.start_at ORDER BY o.start_at,o.event_id`, home, p.Start, p.End, now)
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
		if err = rows.Scan(&v.Start, &v.End, &raw, &fuel); err != nil {
			return nil, nil, err
		}
		v.Fuel = fuel
		var ok bool
		v.Wh, ok = new(big.Rat).SetString(raw)
		if !ok {
			return nil, nil, fmt.Errorf("invalid persisted energy decimal")
		}
		items = append(items, v)
		if asOf == nil || v.End.After(*asOf) {
			t := v.End
			asOf = &t
		}
	}
	return items, asOf, rows.Err()
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
 SELECT max(o.end_at) FROM latest l
 JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision
 JOIN attachments a ON a.id=o.attachment_revision_id AND a.home_id=$1
 WHERE l.event_kind='energy_interval' AND o.end_at <= $2 AND o.end_at > o.start_at`, home, now).Scan(&asOf)
	if asOf != nil {
		utc := asOf.UTC()
		asOf = &utc
	}
	return asOf, err
}

// readMarginalTariff returns the latest revision identity for snapshot
// invalidation, plus a usable rate only when exactly one fresh, current
// electricity tariff covers every measured observation in the response.
func readMarginalTariff(ctx context.Context, tx pgx.Tx, home string, now time.Time, items []interval) (string, int64, *marginalTariff, error) {
	var latestID string
	var latestRevision int64
	err := tx.QueryRow(ctx, `SELECT id::text,revision FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity' ORDER BY revision DESC LIMIT 1`, home).Scan(&latestID, &latestRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		latestID = ""
	} else if err != nil {
		return "", 0, nil, err
	}
	if len(items) == 0 {
		return latestID, latestRevision, nil, nil
	}
	var first, last time.Time
	for i, item := range items {
		if item.Fuel == nil || *item.Fuel != "electricity" {
			return latestID, latestRevision, nil, nil
		}
		if i == 0 || item.Start.Before(first) {
			first = item.Start
		}
		if i == 0 || item.End.After(last) {
			last = item.End
		}
	}
	// Reuse the same ambiguity rule as the energy aggregate. No cost is emitted
	// if any observations overlap, even when their reported values agree.
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if items[i].Start.Before(items[j].End) && items[j].Start.Before(items[i].End) {
				return latestID, latestRevision, nil, nil
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,revision,rate::text,currency,effective_from,effective_to,source_as_of
FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity' AND rate_unit='kWh'
AND rate_kind IN ('user_tariff','official_tariff') AND NOT is_fallback
AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
AND source_as_of <= $2 AND source_as_of > $2 - interval '1440 hours'`, home, now)
	if err != nil {
		return latestID, latestRevision, nil, err
	}
	defer rows.Close()
	var selected *marginalTariff
	count := 0
	for rows.Next() {
		var t marginalTariff
		var rate string
		if err = rows.Scan(&t.ID, &t.Revision, &rate, &t.Currency, &t.From, &t.To, &t.SourceAsOf); err != nil {
			return latestID, latestRevision, nil, err
		}
		t.Rate, _ = new(big.Rat).SetString(rate)
		count++
		selected = &t
	}
	if err = rows.Err(); err != nil {
		return latestID, latestRevision, nil, err
	}
	if count != 1 || selected.From.After(first) || selected.To != nil && selected.To.Before(last) {
		return latestID, latestRevision, nil, nil
	}
	// A recently ended revision may still overlap measured intervals even
	// though it is no longer current. In that case the rate for those
	// intervals is ambiguous; leave cost unknown.
	var historicalOverlap bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tariff_revisions
WHERE home_id=$1 AND id<>$2 AND fuel='electricity' AND rate_unit='kWh'
AND rate_kind IN ('user_tariff','official_tariff') AND NOT is_fallback
AND effective_from < $3 AND (effective_to IS NULL OR effective_to > $4)
AND source_as_of <= $5 AND source_as_of > $5 - interval '1440 hours')`, home, selected.ID, last, first, now).Scan(&historicalOverlap)
	if err != nil {
		return latestID, latestRevision, nil, err
	}
	if historicalOverlap {
		return latestID, latestRevision, nil, nil
	}
	return latestID, latestRevision, selected, nil
}

func priceBuckets(buckets []bucket, t marginalTariff) *money {
	total := new(big.Rat)
	priced := false
	for i := range buckets {
		energy := buckets[i].Allocation.Energy
		if energy == nil {
			continue
		}
		kwh, ok := new(big.Rat).SetString(energy.Value)
		if !ok {
			continue
		}
		amount := new(big.Rat).Mul(kwh, t.Rate)
		rounded, _ := new(big.Rat).SetString(amount.FloatString(2))
		buckets[i].Allocation.Cost = money{Amount: amount.FloatString(2), Currency: t.Currency}
		buckets[i].Allocation.UnclassifiedCost = money{Amount: amount.FloatString(2), Currency: t.Currency}
		buckets[i].Allocation.EvidenceBasis = "mixed"
		total.Add(total, rounded)
		priced = true
	}
	if !priced {
		return nil
	}
	return &money{Amount: total.FloatString(2), Currency: t.Currency}
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
		var wh *big.Rat
		selected := []interval{}
		for _, v := range items {
			if !v.Start.Before(start) && !v.End.After(end) {
				selected = append(selected, v)
				if wh == nil {
					wh = new(big.Rat)
				}
				wh.Add(wh, v.Wh)
			}
		}
		// Distinct meters can include nested circuits and plugs; even one meter
		// can submit conflicting intervals. Never add overlapping measurements.
		for i := 0; i < len(selected); i++ {
			for j := i + 1; j < len(selected); j++ {
				if selected[i].Start.Before(selected[j].End) && selected[j].Start.Before(selected[i].End) {
					wh = nil
				}
			}
		}
		var kwh *big.Rat
		if wh != nil {
			kwh = new(big.Rat).Quo(wh, big.NewRat(1000, 1))
		}
		out = append(out, bucket{Start: start, End: end, Allocation: fromKWh(kwh)})
		start = end
	}
	return out
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
