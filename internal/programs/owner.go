package programs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrOwnerHomeNotFound = errors.New("owner home or appliance not found")
var ErrLocationRequired = errors.New("home location required")

type OwnerProgram struct {
	ProgramID         string   `json:"program_id"`
	RevisionID        string   `json:"revision_id"`
	Title             string   `json:"title"`
	AdministratorName string   `json:"administrator_name"`
	BenefitKind       string   `json:"benefit_kind"`
	Status            string   `json:"status"`
	Reasons           []string `json:"reasons"`
	SourceURL         string   `json:"source_url"`
	SourceSHA256      string   `json:"source_sha256"`
	ReviewExpiresAt   string   `json:"review_expires_at"`
}

type OwnerCoverageScope struct {
	Kind         string     `json:"kind"`
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	ProviderRole *string    `json:"provider_role"`
	Status       string     `json:"status"`
	CheckedAt    *time.Time `json:"checked_at"`
	RetryAfter   *time.Time `json:"retry_after"`
	FailureCode  *string    `json:"failure_code"`
}

// A run covers only scopes identified by its resolver. It does not establish
// complete ZIP provider discovery or service at the customer's address.
type OwnerCoverage struct {
	RunID                string               `json:"run_id"`
	Category             *string              `json:"category"`
	State                string               `json:"state"`
	StartedAt            time.Time            `json:"started_at"`
	FinishedAt           *time.Time           `json:"finished_at"`
	Limitations          []string             `json:"limitations"`
	ResolutionProvenance map[string]any       `json:"resolution_provenance"`
	Scopes               []OwnerCoverageScope `json:"scopes"`
}

type OwnerProgramResult struct {
	Programs []OwnerProgram
	Coverage *OwnerCoverage
}

type OwnerStore interface {
	List(context.Context, string, string, string, time.Time) (OwnerProgramResult, error)
}
type PGOwnerStore struct{ Pool *pgxpool.Pool }

// List reads the owned home, current appliance and reviewed public terms in one
// snapshot. It intentionally omits benefit figures and unstructured revisions.
func (s PGOwnerStore) List(ctx context.Context, owner, home, appliance string, now time.Time) (OwnerProgramResult, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OwnerProgramResult{}, err
	}
	defer tx.Rollback(ctx)
	var country, zip, zone, category, fuel string
	err = tx.QueryRow(ctx, `SELECT h.timezone,a.category,coalesce(a.identity->>'fuel','')
 FROM homes h JOIN appliances a ON a.home_id=h.id AND a.id=$3 AND a.archived_at IS NULL
 WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner, appliance).Scan(&zone, &category, &fuel)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnerProgramResult{}, ErrOwnerHomeNotFound
	}
	if err != nil {
		return OwnerProgramResult{}, err
	}
	err = tx.QueryRow(ctx, `SELECT country_code,postal_code FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&country, &zip)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnerProgramResult{}, ErrLocationRequired
	}
	if err != nil {
		return OwnerProgramResult{}, err
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return OwnerProgramResult{}, fmt.Errorf("invalid home timezone: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT p.id::text,r.id::text,r.title,p.administrator_name,r.benefit_kind,r.status,
 r.reviewed_at,r.review_expires_at,LEAST(r.review_expires_at,s.terms_review_expires_at),
 snap.source_url,snap.content_sha256,a.postal_codes,a.category,a.fuel,coalesce(a.provider_key,''),
 a.requires_income,a.requires_address,a.additional_conditions,r.application_from,r.application_to
 FROM current_verified_program_revisions r
 JOIN public_programs p ON p.id=r.program_id JOIN program_sources s ON s.id=p.source_id
 JOIN program_source_snapshots snap ON snap.id=r.source_snapshot_id
 JOIN reviewed_program_applicability a ON a.revision_id=r.id
 WHERE a.country_code=$1 AND a.postal_codes @> ARRAY[$2]::text[] AND a.category=$3 AND a.fuel=$4 AND r.status IN ('open','upcoming','waitlist','exhausted','suspended','closed','unknown')
 AND NOT EXISTS (SELECT 1 FROM unnest(ARRAY['country_code','postal_codes','category','fuel','provider_key','provider_role','requires_income','requires_address','additional_conditions']) f
  WHERE NOT EXISTS (SELECT 1 FROM public_program_evidence e WHERE e.revision_id=r.id AND e.snapshot_id=r.source_snapshot_id AND e.field_path='applicability.'||f))
 ORDER BY p.id LIMIT 101`, country, zip, category, fuel)
	if err != nil {
		return OwnerProgramResult{}, err
	}
	defer rows.Close()
	out := []OwnerProgram{}
	for rows.Next() {
		var o OwnerProgram
		var reviewed, expires, effectiveExpires time.Time
		var status string
		var zips []string
		var pCategory, pFuel, provider string
		var income, address, extra bool
		var from, to *time.Time
		if err = rows.Scan(&o.ProgramID, &o.RevisionID, &o.Title, &o.AdministratorName, &o.BenefitKind, &status, &reviewed, &expires, &effectiveExpires, &o.SourceURL, &o.SourceSHA256, &zips, &pCategory, &pFuel, &provider, &income, &address, &extra, &from, &to); err != nil {
			return OwnerProgramResult{}, err
		}
		program := Program{ID: o.ProgramID, Status: status, ZIPs: zips, Category: pCategory, Fuel: pFuel, ProviderID: provider, RequiresIncome: income, RequiresAddress: address, ReviewedAt: reviewed, ReviewExpiresAt: effectiveExpires}
		if from != nil {
			program.StartsOn = from.Format("2006-01-02")
		}
		if to != nil {
			program.EndsOn = to.Format("2006-01-02")
		}
		result := Evaluate(program, Household{ZIP: zip, Category: category, Fuel: fuel}, now.In(loc))
		if result.Status == "ineligible" {
			continue
		}
		if extra {
			result.Status = "unknown"
			result.Reasons = append(result.Reasons, "additional_conditions_unknown")
		}
		// Other purchase/installation/reservation windows still need applicant facts.
		result.Status = "unknown"
		result.Reasons = append(result.Reasons, "application_requirements_unconfirmed")
		o.Status, o.Reasons = result.Status, result.Reasons
		o.ReviewExpiresAt = effectiveExpires.UTC().Format(time.RFC3339)
		out = append(out, o)
	}
	if err = rows.Err(); err != nil {
		return OwnerProgramResult{}, err
	}
	rows.Close()
	if len(out) > 100 {
		return OwnerProgramResult{}, fmt.Errorf("too many reviewed programs")
	}
	coverage, err := readOwnerCoverage(ctx, tx, country, zip, category)
	if err != nil {
		return OwnerProgramResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OwnerProgramResult{}, err
	}
	return OwnerProgramResult{Programs: out, Coverage: coverage}, nil
}

func readOwnerCoverage(ctx context.Context, tx pgx.Tx, country, zip, category string) (*OwnerCoverage, error) {
	var run OwnerCoverage
	var limitations, provenance []byte
	err := tx.QueryRow(ctx, `SELECT id::text,category,state,started_at,finished_at,limitations,resolution_provenance
		FROM program_discovery_runs WHERE country_code=$1 AND postal_code=$2
		AND (category=$3 OR category IS NULL)
		ORDER BY started_at DESC,id DESC LIMIT 1`, country, zip, category).
		Scan(&run.RunID, &run.Category, &run.State, &run.StartedAt, &run.FinishedAt, &limitations, &provenance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(limitations, &run.Limitations); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(provenance, &run.ResolutionProvenance); err != nil {
		return nil, err
	}
	run.Scopes = []OwnerCoverageScope{}
	rows, err := tx.Query(ctx, `SELECT scope_kind,scope_key,scope_name,provider_role,status,checked_at,retry_after,failure_code
		FROM program_discovery_coverage WHERE run_id=$1 ORDER BY scope_kind,scope_name,scope_key,provider_role LIMIT 129`, run.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope OwnerCoverageScope
		if err = rows.Scan(&scope.Kind, &scope.Key, &scope.Name, &scope.ProviderRole, &scope.Status, &scope.CheckedAt, &scope.RetryAfter, &scope.FailureCode); err != nil {
			return nil, err
		}
		run.Scopes = append(run.Scopes, scope)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(run.Scopes) > 128 {
		return nil, fmt.Errorf("too many discovery scopes")
	}
	return &run, nil
}

type OwnerService struct {
	Store  OwnerStore
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s OwnerService) Routes(r chi.Router) { r.With(s.Auth).Get("/homes/{home}/programs", s.list) }
func (s OwnerService) list(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), r.URL.Query().Get("appliance_id")
	if _, err := uuid.Parse(home); err != nil {
		ownerFail(w, http.StatusNotFound, "not_found")
		return
	}
	if _, err := uuid.Parse(appliance); err != nil {
		ownerFail(w, http.StatusBadRequest, "invalid_appliance_id")
		return
	}
	if len(r.URL.Query()) != 1 || len(r.URL.Query()["appliance_id"]) != 1 {
		ownerFail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	now := time.Now().UTC()
	result, err := s.Store.List(r.Context(), s.UserID(r), home, appliance, now)
	if errors.Is(err, ErrOwnerHomeNotFound) {
		ownerFail(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, ErrLocationRequired) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Programs []OwnerProgram `json:"programs"`
			AsOf     string         `json:"as_of"`
			Reason   string         `json:"reason"`
		}{[]OwnerProgram{}, now.Format(time.RFC3339), "location_required"})
		return
	}
	if err != nil {
		slog.Error("program owner read failed", "error", err)
		ownerFail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Programs []OwnerProgram `json:"programs"`
		AsOf     string         `json:"as_of"`
		Coverage *OwnerCoverage `json:"coverage"`
	}{result.Programs, now.Format(time.RFC3339), result.Coverage})
}
func ownerFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
