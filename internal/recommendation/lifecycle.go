package recommendation

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrConflict = errors.New("recommendation transition conflict")
var ErrInvalidTransition = errors.New("invalid recommendation transition")

type LifecycleEvent struct {
	ApplianceID  string     `json:"appliance_id"`
	RuleID       string     `json:"rule_id"`
	RuleVersion  int        `json:"rule_version"`
	State        string     `json:"state"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
}

type LifecycleStore interface {
	Latest(context.Context, string, string) (map[string]LifecycleEvent, error)
	Replay(context.Context, string, string, string) (*LifecycleEvent, error)
	Record(context.Context, string, string, string, LifecycleEvent) (LifecycleEvent, error)
}

func lifecycleKey(appliance, rule string) string { return appliance + "\x00" + rule }

// ApplyLifecycle does not turn a stale source into an active recommendation.
// A changed rule version supersedes the prior event; lost active evidence
// expires accepted/snoozed actions until the underlying action is active again.
func ApplyLifecycle(actions []Action, events map[string]LifecycleEvent, now time.Time) []Action {
	for i := range actions {
		event, ok := events[lifecycleKey(actions[i].ApplianceID, actions[i].RuleID)]
		if !ok {
			continue
		}
		if event.RuleVersion != actions[i].RuleVersion {
			actions[i].State = "superseded"
			continue
		}
		if actions[i].State != "active" && !(actions[i].State == "candidate" && (event.State == "snoozed" || event.State == "dismissed")) {
			actions[i].State = "expired"
			continue
		}
		if event.State == "snoozed" && event.SnoozedUntil != nil && !now.Before(*event.SnoozedUntil) {
			continue
		}
		actions[i].State = event.State
	}
	// Keep follow-through visible while moving snoozed/dismissed and stale
	// actions behind current opportunities. Stable order preserves the
	// household evidence ranking within each state.
	sort.SliceStable(actions, func(i, j int) bool { return lifecyclePriority(actions[i].State) < lifecyclePriority(actions[j].State) })
	return actions
}

func lifecyclePriority(state string) int {
	switch state {
	case "accepted":
		return 0
	case "active":
		return 1
	case "candidate":
		return 2
	case "completed":
		return 3
	case "snoozed":
		return 4
	case "dismissed":
		return 5
	default:
		return 6
	}
}

func (s PGStore) Latest(ctx context.Context, owner, home string) (map[string]LifecycleEvent, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (e.appliance_id,e.rule_id) e.appliance_id,e.rule_id,e.rule_version,e.state,e.snoozed_until
 FROM recommendation_events e JOIN appliances a ON a.id=e.appliance_id AND a.home_id=e.home_id
 WHERE e.home_id=$1 AND e.owner_user_id=$2 AND a.archived_at IS NULL
 ORDER BY e.appliance_id,e.rule_id,e.created_at DESC,e.id DESC`, home, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LifecycleEvent{}
	for rows.Next() {
		var e LifecycleEvent
		if err = rows.Scan(&e.ApplianceID, &e.RuleID, &e.RuleVersion, &e.State, &e.SnoozedUntil); err != nil {
			return nil, err
		}
		out[lifecycleKey(e.ApplianceID, e.RuleID)] = e
	}
	return out, rows.Err()
}

// Replay checks ownership before exposing an earlier response. This lets an
// uncertain retry finish even if its recommendation evidence has since aged out.
func (s PGStore) Replay(ctx context.Context, owner, home, key string) (*LifecycleEvent, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var e LifecycleEvent
	err = s.Pool.QueryRow(ctx, `SELECT appliance_id,rule_id,rule_version,state,snoozed_until FROM recommendation_events
	 WHERE home_id=$1 AND owner_user_id=$2 AND idempotency_key=$3`, home, owner, key).
		Scan(&e.ApplianceID, &e.RuleID, &e.RuleVersion, &e.State, &e.SnoozedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Record serializes a household's transitions, validates ownership again and
// deduplicates an uncertain retry by its stable UUID key.
func (s PGStore) Record(ctx context.Context, owner, home, key string, e LifecycleEvent) (LifecycleEvent, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return e, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	var old LifecycleEvent
	err = tx.QueryRow(ctx, `SELECT appliance_id,rule_id,rule_version,state,snoozed_until FROM recommendation_events
 WHERE owner_user_id=$1 AND home_id=$2 AND idempotency_key=$3`, owner, home, key).Scan(&old.ApplianceID, &old.RuleID, &old.RuleVersion, &old.State, &old.SnoozedUntil)
	if err == nil {
		if old.ApplianceID != e.ApplianceID || old.RuleID != e.RuleID || old.RuleVersion != e.RuleVersion || old.State != e.State || !sameTime(old.SnoozedUntil, e.SnoozedUntil) {
			return e, ErrConflict
		}
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM appliances WHERE id=$1 AND home_id=$2 AND archived_at IS NULL FOR UPDATE`, e.ApplianceID, home).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	var previous string
	var previousVersion int
	err = tx.QueryRow(ctx, `SELECT state,rule_version FROM recommendation_events WHERE owner_user_id=$1 AND home_id=$2 AND appliance_id=$3 AND rule_id=$4 ORDER BY created_at DESC,id DESC LIMIT 1`, owner, home, e.ApplianceID, e.RuleID).Scan(&previous, &previousVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	if previousVersion != e.RuleVersion {
		previous = ""
	}
	if !validTransition(previous, e.State) {
		return e, ErrInvalidTransition
	}
	_, err = tx.Exec(ctx, `INSERT INTO recommendation_events(owner_user_id,home_id,appliance_id,rule_id,rule_version,state,snoozed_until,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, owner, home, e.ApplianceID, e.RuleID, e.RuleVersion, e.State, e.SnoozedUntil, key)
	if err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
func validTransition(from, to string) bool {
	switch to {
	case "accepted":
		return from == "" || from == "snoozed" || from == "dismissed"
	case "snoozed", "dismissed":
		return from == "" || from == "accepted" || from == "snoozed"
	case "completed":
		return from == "accepted"
	}
	return false
}
