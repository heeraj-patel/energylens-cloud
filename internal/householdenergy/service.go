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
	Sample *SampleProvider
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
	Start            time.Time  `json:"start_local"`
	End              time.Time  `json:"end_exclusive_local"`
	Allocation       allocation `json:"allocation"`
	CoverageFraction any        `json:"coverage_fraction"`
}
type interval struct {
	Start, End time.Time
	Wh         *big.Rat
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/dashboard", s.dashboard)
	r.With(s.Auth).Get("/homes/{home}/energy-trends", s.trends)
}

func (s Service) dashboard(w http.ResponseWriter, r *http.Request) { s.read(w, r, "dashboard", "hour") }
func (s Service) trends(w http.ResponseWriter, r *http.Request) {
	g := r.URL.Query().Get("granularity")
	if len(r.URL.Query()) != 1 || g != "hour" && g != "day" && g != "week" && g != "month" {
		fail(w, 400, "invalid_request")
		return
	}
	s.read(w, r, "energy_trends", g)
}

func (s Service) read(w http.ResponseWriter, r *http.Request, view, granularity string) {
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
	err = tx.QueryRow(r.Context(), `SELECT timezone,version FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, s.UserID(r)).Scan(&tz, &homeVersion)
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
	var watermark int64
	err = tx.QueryRow(r.Context(), `SELECT COALESCE(max(cursor),0) FROM ingest_batches WHERE home_id=$1`, home).Scan(&watermark)
	if err != nil {
		internal(w, err)
		return
	}
	items, asOf, err := readIntervals(r.Context(), tx, home, p, now)
	if err != nil {
		internal(w, err)
		return
	}
	buckets := makeBuckets(p, granularity, items)
	sample := len(items) == 0 && s.Sample != nil && s.Sample.AllowedUsers[s.UserID(r)] && s.Sample.AllowedHomes[home]
	if sample {
		buckets = sampleBuckets(p, granularity)
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
		a = sampleTotal(len(buckets))
	}
	b := base{SchemaRevision: "household_energy_v1", ContentRevision: "household_energy_backend_v1", HomeID: home, DataMode: "household", GeneratedAt: now, AsOf: asOf, Timezone: tz, Period: p, Scope: "attributed_devices", Availability: a.Availability, EvidenceBasis: a.EvidenceBasis}
	if sample {
		b.DataMode = "sample"
		b.ContentRevision = SampleRevision
		b.FixtureRevision = SampleRevision
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s|%s", home, tz, p.Start.UTC().Format(time.RFC3339Nano), p.End.UTC().Format(time.RFC3339Nano), homeVersion, watermark, granularity, b.DataMode)))
	b.SnapshotID = hex.EncodeToString(digest[:])
	if err = tx.Commit(r.Context()); err != nil {
		internal(w, err)
		return
	}
	var out any
	if view == "dashboard" {
		out = struct {
			base
			View                string     `json:"view"`
			Totals              allocation `json:"totals"`
			TrendPeriod         string     `json:"trend_period"`
			TopRecommendationID any        `json:"top_recommendation_id"`
			AssistantIntentID   any        `json:"assistant_intent_id"`
		}{b, "dashboard", a, "day", nil, nil}
	} else {
		out = struct {
			base
			View        string     `json:"view"`
			Granularity string     `json:"granularity"`
			Totals      allocation `json:"totals"`
			Buckets     []bucket   `json:"buckets"`
		}{b, "energy_trends", granularity, a, buckets}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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

func readIntervals(ctx context.Context, tx pgx.Tx, home string, p period, now time.Time) ([]interval, *time.Time, error) {
	rows, err := tx.Query(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id) e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.event_kind
 FROM ingest_events e WHERE e.home_id=$1 AND e.event_kind='energy_interval' ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC)
	 SELECT o.start_at,o.end_at,o.energy_wh::numeric::text FROM latest l
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
		if err = rows.Scan(&v.Start, &v.End, &raw); err != nil {
			return nil, nil, err
		}
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
		out = append(out, bucket{start, end, fromKWh(kwh), nil})
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
