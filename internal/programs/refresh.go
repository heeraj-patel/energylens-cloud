package programs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRefreshConflict = errors.New("refresh idempotency conflict")
var ErrRefreshLimit = errors.New("too many pending refresh requests")

type RefreshStatus struct {
	RequestID  string         `json:"request_id"`
	State      string         `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at"`
	Coverage   *OwnerCoverage `json:"coverage"`
}

type RefreshStore struct{ Pool *pgxpool.Pool }

// Enqueue snapshots the owner's current appliance and location in one transaction.
// Jobs contain only coarse location/category data and can be shared across owners.
func (s RefreshStore) Enqueue(ctx context.Context, owner, home, appliance, key string) (RefreshStatus, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RefreshStatus{}, err
	}
	defer tx.Rollback(ctx)
	id, err := EnqueueInTx(ctx, tx, owner, home, appliance, key)
	if err != nil {
		return RefreshStatus{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RefreshStatus{}, err
	}
	return s.Status(ctx, owner, home, id)
}

// EnqueueInTx queues a coalesced discovery job inside the caller's transaction
// and returns the request id. It never contacts an external source.
func EnqueueInTx(ctx context.Context, tx pgx.Tx, owner, home, appliance, key string) (string, error) {
	var err error
	var country, zip, category string
	var revision int
	err = tx.QueryRow(ctx, `SELECT l.country_code,l.postal_code,l.revision,a.category
 FROM homes h JOIN appliances a ON a.home_id=h.id AND a.id=$3 AND a.archived_at IS NULL
 LEFT JOIN LATERAL (SELECT country_code,postal_code,revision FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l ON true
 WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL FOR SHARE OF h,a`, home, owner, appliance).Scan(&country, &zip, &revision, &category)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOwnerHomeNotFound
	}
	if err != nil { // NULL location produces scan failure; check explicitly below on a second narrow read.
		var exists bool
		checkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes h JOIN appliances a ON a.home_id=h.id WHERE h.id=$1 AND h.owner_user_id=$2 AND a.id=$3 AND a.archived_at IS NULL AND h.deleted_at IS NULL)`, home, owner, appliance).Scan(&exists)
		if checkErr == nil && exists {
			return "", ErrLocationRequired
		}
		return "", err
	}
	// Serialize a home's requests so its active queue bound and idempotency
	// decision stay stable under concurrent API instances.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "program-refresh:"+owner+":"+home); err != nil {
		return "", err
	}
	var existing string
	var existingAppliance string
	err = tx.QueryRow(ctx, `SELECT id::text,appliance_id::text FROM program_discovery_refresh_requests WHERE owner_user_id=$1 AND home_id=$2 AND idempotency_key=$3`, owner, home, key).Scan(&existing, &existingAppliance)
	if err == nil {
		if existingAppliance != appliance {
			return "", ErrRefreshConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM program_discovery_refresh_requests r JOIN program_discovery_refresh_jobs j ON j.id=r.job_id WHERE r.owner_user_id=$1 AND r.home_id=$2 AND j.state IN ('pending','running')`, owner, home).Scan(&count); err != nil {
		return "", err
	}
	if count >= 10 {
		return "", ErrRefreshLimit
	}
	var jobID string
	for attempt := 0; attempt < 2; attempt++ {
		err = tx.QueryRow(ctx, `SELECT id::text FROM program_discovery_refresh_jobs WHERE country_code=$1 AND postal_code=$2 AND category=$3 AND state IN ('pending','running') ORDER BY created_at LIMIT 1`, country, zip, category).Scan(&jobID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		err = tx.QueryRow(ctx, `INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category) VALUES($1,$2,$3) ON CONFLICT DO NOTHING RETURNING id::text`, country, zip, category).Scan(&jobID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	if jobID == "" {
		return "", errors.New("active refresh job unavailable")
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO program_discovery_refresh_requests(owner_user_id,home_id,appliance_id,location_revision,country_code,postal_code,category,idempotency_key,job_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`, owner, home, appliance, revision, country, zip, category, key, jobID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Status checks all private snapshot fields against the current owner scope.
func (s RefreshStore) Status(ctx context.Context, owner, home, id string) (RefreshStatus, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RefreshStatus{}, err
	}
	defer tx.Rollback(ctx)
	var st RefreshStatus
	var country, zip, category string
	var runID *string
	err = tx.QueryRow(ctx, `SELECT r.id::text,j.state,r.created_at,j.finished_at,j.run_id::text,r.country_code,r.postal_code,r.category
 FROM program_discovery_refresh_requests r JOIN program_discovery_refresh_jobs j ON j.id=r.job_id
 JOIN homes h ON h.id=r.home_id AND h.owner_user_id=r.owner_user_id AND h.deleted_at IS NULL
 JOIN appliances a ON a.id=r.appliance_id AND a.home_id=h.id AND a.archived_at IS NULL AND a.category=r.category
 JOIN LATERAL (SELECT country_code,postal_code,revision FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l
 ON l.revision=r.location_revision AND l.country_code=r.country_code AND l.postal_code=r.postal_code
 WHERE r.id=$1 AND r.owner_user_id=$2 AND r.home_id=$3`, id, owner, home).Scan(&st.RequestID, &st.State, &st.CreatedAt, &st.FinishedAt, &runID, &country, &zip, &category)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshStatus{}, ErrOwnerHomeNotFound
	}
	if err != nil {
		return RefreshStatus{}, err
	}
	if runID != nil {
		st.Coverage, err = readCoverageRun(ctx, tx, *runID)
		if err != nil {
			return RefreshStatus{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return RefreshStatus{}, err
	}
	return st, nil
}

func readCoverageRun(ctx context.Context, tx pgx.Tx, id string) (*OwnerCoverage, error) {
	var c OwnerCoverage
	var limitations, provenance []byte
	err := tx.QueryRow(ctx, `SELECT id::text,category,state,started_at,finished_at,limitations,resolution_provenance FROM program_discovery_runs WHERE id=$1 AND finished_at IS NOT NULL`, id).Scan(&c.RunID, &c.Category, &c.State, &c.StartedAt, &c.FinishedAt, &limitations, &provenance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(limitations, &c.Limitations); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(provenance, &c.ResolutionProvenance); err != nil {
		return nil, err
	}
	c.Scopes = []OwnerCoverageScope{}
	rows, err := tx.Query(ctx, `SELECT scope_kind,scope_key,scope_name,provider_role,status,checked_at,retry_after,failure_code FROM program_discovery_coverage WHERE run_id=$1 ORDER BY scope_kind,scope_name,scope_key,provider_role LIMIT 129`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope OwnerCoverageScope
		if err = rows.Scan(&scope.Kind, &scope.Key, &scope.Name, &scope.ProviderRole, &scope.Status, &scope.CheckedAt, &scope.RetryAfter, &scope.FailureCode); err != nil {
			return nil, err
		}
		c.Scopes = append(c.Scopes, scope)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(c.Scopes) > 128 {
		return nil, errors.New("too many discovery scopes")
	}
	return &c, nil
}

func (s OwnerService) refreshStore() RefreshStore {
	if p, ok := s.Store.(PGOwnerStore); ok {
		return RefreshStore{Pool: p.Pool}
	}
	return RefreshStore{}
}
func (s OwnerService) refresh(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		ownerFail(w, 404, "not_found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	defer r.Body.Close()
	var body struct {
		ApplianceID string `json:"appliance_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		ownerFail(w, 400, "invalid_body")
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		ownerFail(w, 400, "invalid_body")
		return
	}
	if _, err := uuid.Parse(body.ApplianceID); err != nil {
		ownerFail(w, 400, "invalid_appliance_id")
		return
	}
	if _, err := uuid.Parse(r.Header.Get("Idempotency-Key")); err != nil {
		ownerFail(w, 400, "invalid_idempotency_key")
		return
	}
	store := s.refreshStore()
	if store.Pool == nil {
		ownerFail(w, 500, "internal_error")
		return
	}
	st, err := store.Enqueue(r.Context(), s.UserID(r), home, body.ApplianceID, r.Header.Get("Idempotency-Key"))
	switch {
	case errors.Is(err, ErrOwnerHomeNotFound):
		ownerFail(w, 404, "not_found")
		return
	case errors.Is(err, ErrLocationRequired):
		ownerFail(w, 409, "location_required")
		return
	case errors.Is(err, ErrRefreshConflict):
		ownerFail(w, 409, "idempotency_conflict")
		return
	case errors.Is(err, ErrRefreshLimit):
		ownerFail(w, 429, "refresh_limit")
		return
	case err != nil:
		slog.Error("program refresh enqueue failed", "error", err)
		ownerFail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(st)
}
func (s OwnerService) refreshStatus(w http.ResponseWriter, r *http.Request) {
	home, id := chi.URLParam(r, "home"), chi.URLParam(r, "request")
	if _, err := uuid.Parse(home); err != nil {
		ownerFail(w, 404, "not_found")
		return
	}
	if _, err := uuid.Parse(id); err != nil {
		ownerFail(w, 404, "not_found")
		return
	}
	store := s.refreshStore()
	if store.Pool == nil {
		ownerFail(w, 500, "internal_error")
		return
	}
	st, err := store.Status(r.Context(), s.UserID(r), home, id)
	if errors.Is(err, ErrOwnerHomeNotFound) {
		ownerFail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("program refresh status failed", "error", err)
		ownerFail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
