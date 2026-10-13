// Package devicestate projects current metered reachability for the owner
// Devices page. It does not classify useful energy or waste.
package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The installed Hub sync timer runs about every ten minutes with jitter.
// Allow one delayed cycle before treating a reading as unreachable.
const freshFor = 15 * time.Minute

var ErrNotFound = errors.New("home not found")

type State struct {
	ApplianceID string     `json:"appliance_id"`
	State       string     `json:"state"`
	ObservedAt  *time.Time `json:"observed_at,omitempty"`
	Watts       *float64   `json:"watts,omitempty"`
}

type Reader interface {
	Current(context.Context, string, string, time.Time) ([]State, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

type Service struct {
	Store  Reader
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
	Now    func() time.Time
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/device-states", s.list)
}

func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	id, err := uuid.Parse(home)
	if err != nil || id.String() != home {
		http.NotFound(w, r)
		return
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	states, err := s.Store.Current(r.Context(), s.UserID(r), home, now)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("device state read failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		HomeID string    `json:"home_id"`
		AsOf   time.Time `json:"as_of"`
		States []State   `json:"states"`
	}{home, now, states})
}

func classify(observed *time.Time, watts *float64, now time.Time) string {
	if observed == nil || watts == nil || observed.After(now) || now.Sub(*observed) > freshFor {
		return "standby"
	}
	if *watts > 0 {
		return "active"
	}
	return "idle"
}

// Current selects each event's latest revision before finding the newest point.
// Only a single active terminal meter for the current Hub epoch may classify
// an appliance. Missing/stale telemetry is Standby, without implying zero use.
func (s PGStore) Current(ctx context.Context, owner, home string, now time.Time) ([]State, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, home, owner).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := tx.Query(ctx, `WITH attached AS (
	 SELECT a.appliance_id,a.sensor_id,a.started_at,s.hub_id,s.ownership_epoch,
	 count(*) OVER(PARTITION BY a.appliance_id) AS meter_count
	 FROM attachments a JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id
	 JOIN hubs h ON h.id=s.hub_id AND h.home_id=s.home_id AND h.ownership_epoch=s.ownership_epoch AND h.revoked_at IS NULL
	 WHERE a.home_id=$1 AND a.ended_at IS NULL AND s.archived_at IS NULL
	 AND s.kind IN ('plug','strip_outlet') AND 'power'=ANY(s.capabilities)
	), latest AS (
	 SELECT DISTINCT ON (e.hub_id,e.ownership_epoch,e.event_id)
	 e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.sensor_id,e.event_kind
	 FROM ingest_events e WHERE e.home_id=$1
	 ORDER BY e.hub_id,e.ownership_epoch,e.event_id,e.revision DESC
	)
	 SELECT a.id,p.observed_at,p.watts FROM appliances a
	 LEFT JOIN attached t ON t.appliance_id=a.id AND t.meter_count=1
	 LEFT JOIN LATERAL (
	  SELECT o.observed_at,o.watts FROM latest l
	  JOIN energy_observations o ON (o.hub_id,o.ownership_epoch,o.event_id,o.revision)=(l.hub_id,l.ownership_epoch,l.event_id,l.revision)
	  WHERE l.sensor_id=t.sensor_id AND l.hub_id=t.hub_id AND l.ownership_epoch=t.ownership_epoch
	   AND l.event_kind='power_point' AND o.kind='power_point' AND o.observed_at>=t.started_at
	  ORDER BY o.observed_at DESC,l.event_id DESC LIMIT 1
	 ) p ON true
	 WHERE a.home_id=$1 AND a.archived_at IS NULL ORDER BY a.created_at,a.id`, home)
	if err != nil {
		return nil, err
	}
	states := []State{}
	seen := map[string]bool{}
	for rows.Next() {
		var state State
		if err = rows.Scan(&state.ApplianceID, &state.ObservedAt, &state.Watts); err != nil {
			break
		}
		if seen[state.ApplianceID] {
			err = errors.New("duplicate device state")
			break
		}
		seen[state.ApplianceID] = true
		state.State = classify(state.ObservedAt, state.Watts, now)
		if state.State == "standby" {
			state.ObservedAt, state.Watts = nil, nil
		}
		states = append(states, state)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return states, nil
}
