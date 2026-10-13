package programs

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrOwnerHomeNotFound = errors.New("owner home or appliance not found")
var ErrLocationRequired = errors.New("home location required")

type OwnerProgram struct {
	ProgramID          string          `json:"program_id"`
	RevisionID         string          `json:"revision_id"`
	Revision           int64           `json:"-"`
	Title              string          `json:"title"`
	AdministratorName  string          `json:"administrator_name"`
	AdministratorKind  string          `json:"administrator_kind"`
	BenefitKind        string          `json:"benefit_kind"`
	BenefitDisplay     string          `json:"benefit_display"`
	BenefitTerms       json.RawMessage `json:"benefit_terms"`
	EligibilityTerms   json.RawMessage `json:"eligibility_terms"`
	StackingTerms      json.RawMessage `json:"stacking_terms"`
	AvailabilityStatus string          `json:"availability_status"`
	Status             string          `json:"status"`
	Reasons            []string        `json:"reasons"`
	SourceURL          string          `json:"source_url"`
	SourceSHA256       string          `json:"source_sha256"`
	ReviewExpiresAt    string          `json:"review_expires_at"`
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
	Programs      []OwnerProgram
	Coverage      *OwnerCoverage
	ResearchFacts []datawizfacts.Fact
}

type OwnerStore interface {
	List(context.Context, string, string, string, time.Time) (OwnerProgramResult, error)
	ListHome(context.Context, string, string, time.Time) (OwnerProgramResult, error)
}
type PGOwnerStore struct{ Pool *pgxpool.Pool }

// List reads the owned home, current appliance and reviewed public terms in one
// snapshot. Reviewed terms are source-backed data, not an award estimate.
func (s PGOwnerStore) List(ctx context.Context, owner, home, appliance string, now time.Time) (OwnerProgramResult, error) {
	return s.list(ctx, owner, home, appliance, now)
}

// ListHome includes reviewed household offers and offers matching at least one
// current appliance. It does not infer household eligibility or an award.
func (s PGOwnerStore) ListHome(ctx context.Context, owner, home string, now time.Time) (OwnerProgramResult, error) {
	return s.list(ctx, owner, home, "", now)
}

func (s PGOwnerStore) list(ctx context.Context, owner, home, appliance string, now time.Time) (OwnerProgramResult, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OwnerProgramResult{}, err
	}
	defer tx.Rollback(ctx)
	var country, zip, zone, category, fuel, manufacturer string
	if appliance == "" {
		err = tx.QueryRow(ctx, `SELECT h.timezone FROM homes h
 WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner).Scan(&zone)
	} else {
		err = tx.QueryRow(ctx, `SELECT h.timezone,a.category,coalesce(a.identity->>'fuel',''),coalesce(a.identity->>'brand','')
 FROM homes h JOIN appliances a ON a.home_id=h.id AND a.id=$3 AND a.archived_at IS NULL
 WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner, appliance).Scan(&zone, &category, &fuel, &manufacturer)
	}
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
	coverage, err := readOwnerCoverage(ctx, tx, country, zip, category)
	if err != nil {
		return OwnerProgramResult{}, err
	}
	match := `a.category=$3 AND a.fuel=$4`
	args := []any{country, zip, category, fuel}
	if appliance == "" {
		match = `(a.category='home' AND a.fuel='any' OR EXISTS (
 SELECT 1 FROM appliances device WHERE device.home_id=$3 AND device.archived_at IS NULL
 AND device.category=a.category AND coalesce(device.identity->>'fuel','')=a.fuel))`
		args = []any{country, zip, home}
	}
	query := `SELECT p.id::text,r.id::text,r.revision,r.title,p.administrator_name,r.administrator_kind,r.benefit_kind,r.benefit_display,r.benefit_terms,r.eligibility_terms,r.stacking_terms,r.status,
 r.reviewed_at,r.review_expires_at,LEAST(r.review_expires_at,s.terms_review_expires_at),
 snap.source_url,snap.content_sha256,a.postal_codes,a.geography_scope,a.category,a.fuel,coalesce(a.provider_key,''),coalesce(a.provider_role,''),
	 a.requires_income,a.requires_address,a.additional_conditions,r.application_from,r.application_to,
	 (r.purchase_from IS NOT NULL OR r.purchase_to IS NOT NULL OR r.installation_from IS NOT NULL OR
	 r.installation_to IS NOT NULL OR r.reservation_from IS NOT NULL OR r.reservation_to IS NOT NULL)
 FROM current_verified_program_revisions r
 JOIN public_programs p ON p.id=r.program_id JOIN program_sources s ON s.id=p.source_id
 JOIN program_source_snapshots snap ON snap.id=r.source_snapshot_id
 JOIN reviewed_program_applicability a ON a.revision_id=r.id
	WHERE a.country_code=$1 AND (a.geography_scope='country' OR a.postal_codes @> ARRAY[$2]::text[]) AND ` + match + ` AND r.status IN ('open','upcoming','waitlist','exhausted','suspended','closed','unknown')
 AND NOT EXISTS (SELECT 1 FROM unnest(ARRAY['country_code','postal_codes','category','fuel','provider_key','provider_role','requires_income','requires_address','additional_conditions']) f
  WHERE NOT EXISTS (SELECT 1 FROM public_program_evidence e WHERE e.revision_id=r.id AND e.snapshot_id=r.source_snapshot_id AND e.field_path='applicability.'||f))
	AND (a.geography_scope <> 'country' OR EXISTS (SELECT 1 FROM public_program_evidence e WHERE e.revision_id=r.id AND e.snapshot_id=r.source_snapshot_id AND e.field_path='applicability.geography_scope'))
	AND NOT EXISTS (SELECT 1 FROM unnest(ARRAY['administrator_kind','title','status','benefit_kind','benefit_display','benefit_terms','eligibility_terms','stacking_terms']) f
  WHERE NOT EXISTS (SELECT 1 FROM public_program_evidence e WHERE e.revision_id=r.id AND e.snapshot_id=r.source_snapshot_id AND e.field_path=f))
 ORDER BY p.id LIMIT 101`
	rows, err := tx.Query(ctx, query, args...)
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
		var geographyScope, pCategory, pFuel, provider, providerRole string
		var income, address, extra bool
		var otherWindows bool
		var from, to *time.Time
		if err = rows.Scan(&o.ProgramID, &o.RevisionID, &o.Revision, &o.Title, &o.AdministratorName, &o.AdministratorKind, &o.BenefitKind, &o.BenefitDisplay, &o.BenefitTerms, &o.EligibilityTerms, &o.StackingTerms, &status, &reviewed, &expires, &effectiveExpires, &o.SourceURL, &o.SourceSHA256, &zips, &geographyScope, &pCategory, &pFuel, &provider, &providerRole, &income, &address, &extra, &from, &to, &otherWindows); err != nil {
			return OwnerProgramResult{}, err
		}
		program := Program{ID: o.ProgramID, Status: status, ZIPs: zips, Category: pCategory, Fuel: pFuel, ProviderID: provider, RequiresIncome: income, RequiresAddress: address, ReviewedAt: reviewed, ReviewExpiresAt: effectiveExpires}
		if from != nil {
			program.StartsOn = from.Format("2006-01-02")
		}
		if to != nil {
			program.EndsOn = to.Format("2006-01-02")
		}
		// ZIP discovery identifies a provider candidate, never confirmed service
		// at the home's address. Match the reviewed provider role as well as key.
		candidate := false
		if coverage != nil {
			for _, scope := range coverage.Scopes {
				if scope.Kind == "utility" && scope.Key == provider && scope.ProviderRole != nil && *scope.ProviderRole == providerRole {
					candidate = true
					break
				}
			}
		}
		if geographyScope == "country" {
			program.ZIPs = nil
		}
		householdCategory, householdFuel := category, fuel
		if appliance == "" {
			householdCategory, householdFuel = pCategory, pFuel // SQL proved an active device match or household scope.
		}
		result := Evaluate(program, Household{ZIP: zip, Category: householdCategory, Fuel: householdFuel, CandidateProvider: map[string]bool{provider: candidate}}, now.In(loc))
		// A source-backed, household-matching program remains part of the feed
		// when its assessment is ineligible. Unknown inventory facts may soften
		// a possible match, but must not erase a known mismatch or closed offer.
		if extra && result.Status != "ineligible" {
			result.Status = "unknown"
			result.Reasons = append(result.Reasons, "additional_conditions_unknown")
		}
		// Purchase, installation and reservation dates require facts that the
		// household inventory does not yet contain.
		if otherWindows && result.Status != "ineligible" {
			result.Status = "unknown"
			result.Reasons = append(result.Reasons, "transaction_windows_unconfirmed")
		}
		o.AvailabilityStatus = status
		o.Status, o.Reasons = result.Status, result.Reasons
		o.ReviewExpiresAt = effectiveExpires.UTC().Format(time.RFC3339)
		out = append(out, o)
	}
	if err = rows.Err(); err != nil {
		return OwnerProgramResult{}, err
	}
	rows.Close()
	sort.SliceStable(out, func(i, j int) bool {
		left, leftOK := fixedOfferSaving(out[i])
		right, rightOK := fixedOfferSaving(out[j])
		if leftOK != rightOK {
			return leftOK
		}
		return leftOK && left.Cmp(right) > 0
	})
	if len(out) > 100 {
		return OwnerProgramResult{}, fmt.Errorf("too many reviewed programs")
	}
	research := []datawizfacts.Fact{}
	if country == "US" {
		zipFacts, readErr := datawizfacts.Current(ctx, tx, "zip", "zip:"+zip)
		if readErr != nil {
			return OwnerProgramResult{}, readErr
		}
		for _, fact := range zipFacts {
			if fact.Field == "government_offer" {
				research = append(research, fact)
			}
		}
		subjects := map[string]bool{}
		if appliance != "" && manufacturer != "" && category != "" {
			subjects[datawizfacts.SavingsSubject(manufacturer, category)] = true
		}
		if appliance == "" {
			deviceRows, readErr := tx.Query(ctx, `SELECT coalesce(identity->>'brand',''),category FROM appliances WHERE home_id=$1 AND archived_at IS NULL`, home)
			if readErr != nil {
				return OwnerProgramResult{}, readErr
			}
			for deviceRows.Next() {
				var brand, cat string
				if readErr = deviceRows.Scan(&brand, &cat); readErr != nil {
					break
				}
				if brand != "" && cat != "" {
					subjects[datawizfacts.SavingsSubject(brand, cat)] = true
				}
			}
			if readErr == nil {
				readErr = deviceRows.Err()
			}
			deviceRows.Close()
			if readErr != nil {
				return OwnerProgramResult{}, readErr
			}
		}
		for subject := range subjects {
			facts, readErr := datawizfacts.Current(ctx, tx, "savings", subject)
			if readErr != nil {
				return OwnerProgramResult{}, readErr
			}
			for _, fact := range facts {
				if fact.Field == "government_offer" || fact.Field == "manufacturer_offer" {
					research = append(research, fact)
				}
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return OwnerProgramResult{}, err
	}
	return OwnerProgramResult{Programs: out, Coverage: coverage, ResearchFacts: research}, nil
}

// Only source-reviewed fixed US-dollar rebates/discounts have a comparable
// purchase saving. Caps, ranges, percentages and recurring amounts do not.
func fixedOfferSaving(program OwnerProgram) (*big.Rat, bool) {
	if program.BenefitKind != "rebate" && program.BenefitKind != "discount" {
		return nil, false
	}
	var terms map[string]json.RawMessage
	if json.Unmarshal(program.BenefitTerms, &terms) != nil {
		return nil, false
	}
	for _, field := range []string{"up_to", "maximum", "minimum", "percentage", "per_unit", "annual", "monthly"} {
		if _, exists := terms[field]; exists {
			return nil, false
		}
	}
	if raw, exists := terms["currency"]; exists && string(raw) != `"USD"` {
		return nil, false
	}
	if raw, exists := terms["period"]; exists && string(raw) != `"one_time"` {
		return nil, false
	}
	raw, exists := terms["amount"]
	if !exists {
		return nil, false
	}
	amount := strings.Trim(string(raw), `"`)
	if amount == "" || strings.HasPrefix(amount, "-") || strings.ContainsAny(amount, "eE") {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(amount)
	return value, ok && value.Sign() > 0
}

func readOwnerCoverage(ctx context.Context, tx pgx.Tx, country, zip, category string) (*OwnerCoverage, error) {
	var run OwnerCoverage
	var limitations, provenance []byte
	err := tx.QueryRow(ctx, `SELECT id::text,category,state,started_at,finished_at,limitations,resolution_provenance
		FROM program_discovery_runs WHERE country_code=$1 AND postal_code=$2
		AND (category=$3 OR category IS NULL) AND finished_at IS NOT NULL
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

func (s OwnerService) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/programs", s.list)
	r.With(s.Auth).Post("/homes/{home}/programs/discovery-refresh", s.refresh)
	r.With(s.Auth).Get("/homes/{home}/programs/discovery-refresh/{request}", s.refreshStatus)
}
func (s OwnerService) list(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), r.URL.Query().Get("appliance_id")
	if _, err := uuid.Parse(home); err != nil {
		ownerFail(w, http.StatusNotFound, "not_found")
		return
	}
	if len(r.URL.Query()) > 1 || (len(r.URL.Query()) == 1 && len(r.URL.Query()["appliance_id"]) != 1) {
		ownerFail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if appliance != "" {
		if _, err := uuid.Parse(appliance); err != nil {
			ownerFail(w, http.StatusBadRequest, "invalid_appliance_id")
			return
		}
	} else if len(r.URL.Query()) != 0 {
		ownerFail(w, http.StatusBadRequest, "invalid_appliance_id")
		return
	}
	now := time.Now().UTC()
	var result OwnerProgramResult
	var err error
	if appliance == "" {
		result, err = s.Store.ListHome(r.Context(), s.UserID(r), home, now)
	} else {
		result, err = s.Store.List(r.Context(), s.UserID(r), home, appliance, now)
	}
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
		Programs      []OwnerProgram      `json:"programs"`
		AsOf          string              `json:"as_of"`
		Coverage      *OwnerCoverage      `json:"coverage"`
		ResearchFacts []datawizfacts.Fact `json:"research_facts,omitempty"`
	}{result.Programs, now.Format(time.RFC3339), result.Coverage, result.ResearchFacts})
}
func ownerFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
