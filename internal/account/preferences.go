package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDisclosureAccepted = errors.New("history disclosure already accepted")
var ErrInvalidDisclosure = errors.New("invalid history disclosure timestamp")

type AccountPreferences struct {
	Version                     int64      `json:"version"`
	SchemaVersion               int        `json:"schema_version"`
	EmailReportsEnabled         bool       `json:"email_reports_enabled"`
	HistoryDisclosureAcceptedAt *time.Time `json:"history_disclosure_accepted_at"`
}
type HomePreferences struct {
	Version         int64  `json:"version"`
	SchemaVersion   int    `json:"schema_version"`
	Goal            string `json:"goal"`
	ComfortPriority string `json:"comfort_priority"`
}
type AccountPreferencesPatch struct {
	EmailReportsEnabled         *bool   `json:"email_reports_enabled,omitempty"`
	HistoryDisclosureAcceptedAt *string `json:"history_disclosure_accepted_at,omitempty"`
}
type HomePreferencesPatch struct {
	Goal            *string `json:"goal,omitempty"`
	ComfortPriority *string `json:"comfort_priority,omitempty"`
}
type PreferenceStore interface {
	GetAccountPreferences(context.Context, string) (AccountPreferences, error)
	PatchAccountPreferences(context.Context, string, AccountPreferencesPatch, int64, string, []byte) (AccountPreferences, error)
	GetHomePreferences(context.Context, string, string) (HomePreferences, error)
	PatchHomePreferences(context.Context, string, string, HomePreferencesPatch, int64, string, []byte) (HomePreferences, error)
}
type PGPreferenceStore struct{ Pool *pgxpool.Pool }

func (s PGPreferenceStore) GetAccountPreferences(ctx context.Context, owner string) (AccountPreferences, error) {
	var p AccountPreferences
	e := s.Pool.QueryRow(ctx, `SELECT version,schema_version,email_reports_enabled,history_disclosure_accepted_at FROM account_preferences WHERE user_id=$1`, owner).Scan(&p.Version, &p.SchemaVersion, &p.EmailReportsEnabled, &p.HistoryDisclosureAcceptedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, e
}
func (s PGPreferenceStore) GetHomePreferences(ctx context.Context, owner, home string) (HomePreferences, error) {
	var p HomePreferences
	e := s.Pool.QueryRow(ctx, `SELECT p.version,p.schema_version,p.goal,p.comfort_priority FROM home_preferences p JOIN homes h ON h.id=p.home_id AND h.owner_user_id=$1 AND h.deleted_at IS NULL WHERE p.home_id=$2`, owner, home).Scan(&p.Version, &p.SchemaVersion, &p.Goal, &p.ComfortPriority)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, e
}
func prefReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte, out any) (bool, error) {
	var oldHash, body []byte
	e := tx.QueryRow(ctx, `SELECT request_hash,response_json FROM preference_idempotency WHERE owner_user_id=$1 AND operation=$2 AND key=$3`, owner, op, key).Scan(&oldHash, &body)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if !bytes.Equal(oldHash, hash) {
		return true, ErrKeyReuse
	}
	return true, json.Unmarshal(body, out)
}
func savePrefReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO preference_idempotency(owner_user_id,operation,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5)`, owner, op, key, hash, b)
	return e
}
func (s PGPreferenceStore) PatchAccountPreferences(ctx context.Context, owner string, in AccountPreferencesPatch, version int64, key string, hash []byte) (AccountPreferences, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return AccountPreferences{}, e
	}
	defer tx.Rollback(ctx)
	var lockedOwner string
	if e = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&lockedOwner); errors.Is(e, pgx.ErrNoRows) {
		return AccountPreferences{}, ErrNotFound
	} else if e != nil {
		return AccountPreferences{}, e
	}
	var old AccountPreferences
	if found, e := prefReplay(ctx, tx, owner, "account_preferences", key, hash, &old); found || e != nil {
		return old, e
	}
	var current AccountPreferences
	e = tx.QueryRow(ctx, `SELECT version,schema_version,email_reports_enabled,history_disclosure_accepted_at FROM account_preferences WHERE user_id=$1 FOR UPDATE`, owner).Scan(&current.Version, &current.SchemaVersion, &current.EmailReportsEnabled, &current.HistoryDisclosureAcceptedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return AccountPreferences{}, ErrNotFound
	}
	if e != nil {
		return AccountPreferences{}, e
	}
	if current.Version != version {
		return AccountPreferences{}, ErrVersion
	}
	if in.HistoryDisclosureAcceptedAt != nil && !validDisclosure(*in.HistoryDisclosureAcceptedAt) {
		return AccountPreferences{}, ErrInvalidDisclosure
	}
	if in.HistoryDisclosureAcceptedAt != nil && current.HistoryDisclosureAcceptedAt != nil {
		return AccountPreferences{}, ErrDisclosureAccepted
	}
	email := current.EmailReportsEnabled
	if in.EmailReportsEnabled != nil {
		email = *in.EmailReportsEnabled
	}
	var accepted any = current.HistoryDisclosureAcceptedAt
	if in.HistoryDisclosureAcceptedAt != nil {
		accepted = time.Now().UTC()
	}
	var p AccountPreferences
	e = tx.QueryRow(ctx, `UPDATE account_preferences SET version=version+1,email_reports_enabled=$2,history_disclosure_accepted_at=$3,updated_at=now() WHERE user_id=$1 RETURNING version,schema_version,email_reports_enabled,history_disclosure_accepted_at`, owner, email, accepted).Scan(&p.Version, &p.SchemaVersion, &p.EmailReportsEnabled, &p.HistoryDisclosureAcceptedAt)
	if e != nil {
		return p, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO account_preference_revisions(user_id,version,schema_version,email_reports_enabled,history_disclosure_accepted_at) VALUES($1,$2,$3,$4,$5)`, owner, p.Version, p.SchemaVersion, p.EmailReportsEnabled, p.HistoryDisclosureAcceptedAt)
	if e != nil {
		return p, e
	}
	if e = savePrefReplay(ctx, tx, owner, "account_preferences", key, hash, p); e != nil {
		return p, e
	}
	return p, tx.Commit(ctx)
}
func (s PGPreferenceStore) PatchHomePreferences(ctx context.Context, owner, home string, in HomePreferencesPatch, version int64, key string, hash []byte) (HomePreferences, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return HomePreferences{}, e
	}
	defer tx.Rollback(ctx)
	var lockedOwner string
	if e = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&lockedOwner); errors.Is(e, pgx.ErrNoRows) {
		return HomePreferences{}, ErrNotFound
	} else if e != nil {
		return HomePreferences{}, e
	}
	// Check the current tenant boundary before returning a stored response. A
	// deleted or reassigned home must not disclose a previous replay payload.
	var activeHome string
	if e = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&activeHome); errors.Is(e, pgx.ErrNoRows) {
		return HomePreferences{}, ErrNotFound
	} else if e != nil {
		return HomePreferences{}, e
	}
	op := "home_preferences:" + home
	var old HomePreferences
	if found, e := prefReplay(ctx, tx, owner, op, key, hash, &old); found || e != nil {
		return old, e
	}
	var p HomePreferences
	e = tx.QueryRow(ctx, `SELECT version,schema_version,goal,comfort_priority FROM home_preferences WHERE home_id=$1 FOR UPDATE`, home).Scan(&p.Version, &p.SchemaVersion, &p.Goal, &p.ComfortPriority)
	if errors.Is(e, pgx.ErrNoRows) {
		return HomePreferences{}, ErrNotFound
	}
	if e != nil {
		return HomePreferences{}, e
	}
	if p.Version != version {
		return HomePreferences{}, ErrVersion
	}
	if in.Goal != nil {
		p.Goal = *in.Goal
	}
	if in.ComfortPriority != nil {
		p.ComfortPriority = *in.ComfortPriority
	}
	e = tx.QueryRow(ctx, `UPDATE home_preferences SET version=version+1,goal=$2,comfort_priority=$3,updated_at=now() WHERE home_id=$1 RETURNING version,schema_version,goal,comfort_priority`, home, p.Goal, p.ComfortPriority).Scan(&p.Version, &p.SchemaVersion, &p.Goal, &p.ComfortPriority)
	if e != nil {
		return p, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO home_preference_revisions(home_id,version,schema_version,goal,comfort_priority) VALUES($1,$2,$3,$4,$5)`, home, p.Version, p.SchemaVersion, p.Goal, p.ComfortPriority)
	if e != nil {
		return p, e
	}
	if e = savePrefReplay(ctx, tx, owner, op, key, hash, p); e != nil {
		return p, e
	}
	return p, tx.Commit(ctx)
}
func prefHash(version int64, body []byte) []byte {
	sum := sha256.Sum256(append([]byte(strconv.FormatInt(version, 10)+"\n"), body...))
	return sum[:]
}
func validDisclosure(s string) bool {
	t, e := time.Parse(time.RFC3339, s)
	return e == nil && t.After(time.Now().Add(-5*time.Minute)) && t.Before(time.Now().Add(1*time.Minute))
}
func validGoal(s string) bool {
	switch s {
	case "reduce_cost", "reduce_energy", "understand_usage", "unspecified":
		return true
	}
	return false
}
func validComfort(s string) bool {
	switch s {
	case "high", "balanced", "flexible", "unknown":
		return true
	}
	return false
}
func prefError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, ErrNotFound):
		fail(w, 404, "not_found", "preferences not found")
	case errors.Is(e, ErrVersion):
		fail(w, 409, "version_conflict", "preference version changed")
	case errors.Is(e, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse", "key used with another request")
	case errors.Is(e, ErrInvalidDisclosure):
		fail(w, 400, "invalid_request", "invalid history disclosure timestamp")
	case errors.Is(e, ErrDisclosureAccepted):
		fail(w, 409, "disclosure_already_accepted", "history disclosure was already accepted")
	default:
		internal(w, fmt.Errorf("preferences: %w", e))
	}
}
func (s Service) accountPreferencesGet(w http.ResponseWriter, r *http.Request) {
	p, e := s.Preferences.GetAccountPreferences(r.Context(), user(r).ID)
	if e != nil {
		prefError(w, e)
		return
	}
	etag(w, p.Version)
	jsonOut(w, 200, p)
}
func (s Service) homePreferencesGet(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found", "home not found")
		return
	}
	p, e := s.Preferences.GetHomePreferences(r.Context(), user(r).ID, home)
	if e != nil {
		prefError(w, e)
		return
	}
	etag(w, p.Version)
	jsonOut(w, 200, p)
}
func (s Service) accountPreferencesPatch(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	v, ok := matchVersion(w, r)
	if !ok {
		return
	}
	var in AccountPreferencesPatch
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !validPatchFields(body, "email_reports_enabled", "history_disclosure_accepted_at") || in.EmailReportsEnabled == nil && in.HistoryDisclosureAcceptedAt == nil || in.HistoryDisclosureAcceptedAt != nil && !validDisclosure(*in.HistoryDisclosureAcceptedAt) {
		fail(w, 400, "invalid_request", "invalid account preferences")
		return
	}
	p, e := s.Preferences.PatchAccountPreferences(r.Context(), user(r).ID, in, v, key, prefHash(v, body))
	if e != nil {
		prefError(w, e)
		return
	}
	etag(w, p.Version)
	jsonOut(w, 200, p)
}
func (s Service) homePreferencesPatch(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found", "home not found")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	v, ok := matchVersion(w, r)
	if !ok {
		return
	}
	var in HomePreferencesPatch
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !validPatchFields(body, "goal", "comfort_priority") || in.Goal == nil && in.ComfortPriority == nil || in.Goal != nil && !validGoal(*in.Goal) || in.ComfortPriority != nil && !validComfort(*in.ComfortPriority) {
		fail(w, 400, "invalid_request", "invalid home preferences")
		return
	}
	p, e := s.Preferences.PatchHomePreferences(r.Context(), user(r).ID, home, in, v, key, prefHash(v, body))
	if e != nil {
		prefError(w, e)
		return
	}
	etag(w, p.Version)
	jsonOut(w, 200, p)
}
func validPatchFields(body []byte, allowed ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) == 0 {
		return false
	}
	for k, v := range fields {
		valid := false
		for _, a := range allowed {
			if k == a {
				valid = true
				break
			}
		}
		if !valid || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}
