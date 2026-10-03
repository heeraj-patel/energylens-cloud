package programs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scope is a candidate jurisdiction or provider returned by a ZIP resolver.
// A utility candidate never establishes that a household buys service from it.
type Scope struct {
	Kind, Key, Name, ProviderRole, SourceKey string
}

type ZIPResolver interface {
	ResolveZIP(context.Context, string, string) ([]Scope, error)
}

// DetailedZIPResolver adds evidence for offline datasets without changing
// existing resolvers. Its limitations keep a successful ZIP lookup partial.
type DetailedZIPResolver interface {
	ResolveZIPDetailed(context.Context, string, string) (ZIPResolution, error)
}

type ZIPResolution struct {
	Scopes      []Scope
	Provenance  map[string]any
	Limitations []string
}

// SourceProbe checks one registered public source. It must enforce the source's
// access terms, host allowlist and response budget. A successful probe records
// coverage only; it does not renew reviewed terms or publish program revisions.
type SourceProbe interface {
	Check(context.Context, RegisteredSource) error
}

type SnapshotSourceProbe interface {
	Fetch(context.Context, RegisteredSource) (FetchedSource, error)
}

type RegisteredSource struct {
	ID                uuid.UUID
	Key, CanonicalURL string
	ParserVersion     string
	AllowedHosts      []string
	AccessMethod      string
	MaxResponseBytes  int64
	MaxRequests       int
}

type DiscoveryRunner struct {
	DB       *pgxpool.Pool
	Resolver ZIPResolver
	Probe    SourceProbe // nil leaves source checks pending
}

type DiscoveryRequest struct {
	CountryCode, PostalCode, StateCode, CountyName, CityName, Category string
}

type DiscoveryOutcome struct {
	RunID uuid.UUID
	State string
}

var countryCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)
var failureCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validScope(s Scope) bool {
	if s.Key == "" || s.Name == "" || strings.TrimSpace(s.Key) != s.Key || strings.TrimSpace(s.Name) != s.Name {
		return false
	}
	switch s.Kind {
	case "utility":
		switch s.ProviderRole {
		case "electric_service_candidate", "electric_distributor", "electric_retailer", "gas_distributor", "gas_retailer", "other_fuel":
			return true
		}
	case "city", "county", "state", "federal", "tribal", "manufacturer":
		return s.ProviderRole == ""
	}
	return false
}

// Run records every identified scope, including unresolved and failed checks.
// Resolver failure is persisted as a failed run so absence of coverage is visible.
func (r DiscoveryRunner) Run(ctx context.Context, req DiscoveryRequest) (out DiscoveryOutcome, err error) {
	if r.DB == nil || r.Resolver == nil || !countryCodePattern.MatchString(req.CountryCode) || req.PostalCode == "" || strings.TrimSpace(req.PostalCode) != req.PostalCode {
		return DiscoveryOutcome{}, errors.New("invalid discovery request or runner")
	}
	resolution := ZIPResolution{}
	var resolveErr error
	if detailed, ok := r.Resolver.(DetailedZIPResolver); ok {
		resolution, resolveErr = detailed.ResolveZIPDetailed(ctx, req.CountryCode, req.PostalCode)
	} else {
		resolution.Scopes, resolveErr = r.Resolver.ResolveZIP(ctx, req.CountryCode, req.PostalCode)
	}
	scopes := resolution.Scopes
	if resolveErr != nil {
		// A partial resolver response cannot establish complete ZIP coverage.
		scopes = nil
		resolution.Provenance = nil
		resolution.Limitations = nil
	} else {
		registered, lookupErr := registeredSourceScopes(ctx, r.DB, req)
		if lookupErr != nil {
			return DiscoveryOutcome{}, lookupErr
		}
		scopes = append(scopes, registered...)
		if len(registered) > 0 {
			resolution.Limitations = append(resolution.Limitations, "registered_source_fanout_non_exhaustive")
		}
	}
	if len(scopes) > 128 {
		return DiscoveryOutcome{}, errors.New("ZIP resolver exceeded 128 candidate scopes")
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		key := s.Kind + "\x00" + s.Key + "\x00" + s.ProviderRole
		if !validScope(s) || seen[key] {
			return DiscoveryOutcome{}, fmt.Errorf("invalid or duplicate ZIP scope: %q", s.Key)
		}
		seen[key] = true
	}
	state, limitation := "complete", ""
	if resolveErr != nil {
		state, limitation = "failed", "zip_resolver_failed"
	} else if len(scopes) == 0 {
		state, limitation = "partial", "no_scopes_identified"
	}
	limitations := append([]string{}, resolution.Limitations...)
	if limitation != "" {
		limitations = append(limitations, limitation)
	}
	if len(resolution.Limitations) > 0 {
		state = "partial"
	}
	if len(limitations) > 32 {
		return DiscoveryOutcome{}, errors.New("too many ZIP limitations")
	}
	uniqueLimitations := make(map[string]bool, len(limitations))
	for _, value := range limitations {
		if value == "" || len(value) > 120 || uniqueLimitations[value] {
			return DiscoveryOutcome{}, errors.New("invalid or duplicate ZIP limitation")
		}
		uniqueLimitations[value] = true
	}
	limitationsJSON, err := json.Marshal(limitations)
	if err != nil {
		return DiscoveryOutcome{}, err
	}
	provenanceJSON, err := json.Marshal(resolution.Provenance)
	if err != nil {
		return DiscoveryOutcome{}, err
	}
	if resolution.Provenance == nil {
		provenanceJSON = []byte("{}")
	}
	if len(provenanceJSON) > 4096 || len(resolution.Provenance) > 10 {
		return DiscoveryOutcome{}, errors.New("ZIP provenance exceeds bound")
	}
	var runID uuid.UUID
	err = r.DB.QueryRow(ctx, `INSERT INTO program_discovery_runs
		(country_code,postal_code,state_code,county_name,city_name,category,state,limitations,resolution_provenance)
		VALUES ($1,$2,$3,$4,$5,$6,'running',$7::jsonb,$8::jsonb) RETURNING id`,
		req.CountryCode, req.PostalCode, nullableText(req.StateCode), nullableText(req.CountyName), nullableText(req.CityName), nullableText(req.Category), string(limitationsJSON), string(provenanceJSON)).Scan(&runID)
	if err != nil {
		return DiscoveryOutcome{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = r.DB.Exec(finishCtx, `UPDATE program_discovery_runs SET state='failed',finished_at=now(),limitations=limitations || '["runner_failed"]'::jsonb WHERE id=$1 AND finished_at IS NULL`, runID)
	}()
	// A source is probed once even when several scopes refer to it. This also
	// prevents a single ZIP from multiplying the registered per-run budget.
	type probeResult struct {
		checked bool
		code    string
	}
	checkedSources := map[uuid.UUID]probeResult{}
	for _, s := range scopes {
		status, failureCode := "pending", ""
		var sourceID any
		var checkedAt any
		if s.SourceKey != "" {
			var source RegisteredSource
			var enabled bool
			var reviewedAt, expires time.Time
			e := r.DB.QueryRow(ctx, `SELECT id,source_key,canonical_url,allowed_hosts,access_method,max_response_bytes,max_requests_per_run,enabled,terms_reviewed_at,terms_review_expires_at,parser_version
				FROM program_sources WHERE source_key=$1`, s.SourceKey).Scan(&source.ID, &source.Key, &source.CanonicalURL, &source.AllowedHosts, &source.AccessMethod, &source.MaxResponseBytes, &source.MaxRequests, &enabled, &reviewedAt, &expires, &source.ParserVersion)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return DiscoveryOutcome{}, e
			}
			if e != nil || !enabled || reviewedAt.After(time.Now()) || !expires.After(time.Now()) {
				failureCode = "source_unavailable"
			} else {
				sourceID = source.ID
				if result, ok := checkedSources[source.ID]; ok {
					if result.checked {
						status, checkedAt = "checked", time.Now().UTC()
					} else {
						status, failureCode = "failed", result.code
					}
				} else if r.Probe == nil {
					failureCode = "probe_unconfigured"
				} else {
					probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					var probeErr error
					if fetcher, ok := r.Probe.(SnapshotSourceProbe); ok {
						var fetched FetchedSource
						fetched, probeErr = fetcher.Fetch(probeCtx, source)
						if probeErr == nil {
							probeErr = storeSourceSnapshot(probeCtx, r.DB, source, fetched)
						}
					} else {
						probeErr = r.Probe.Check(probeCtx, source)
					}
					cancel()
					if probeErr == nil {
						status, checkedAt = "checked", time.Now().UTC()
						checkedSources[source.ID] = probeResult{checked: true}
					} else {
						status, failureCode = "failed", "source_check_failed"
						checkedSources[source.ID] = probeResult{code: failureCode}
					}
				}
			}
		}
		if status != "checked" {
			state = "partial"
		}
		if !failureCodePattern.MatchString(failureCode) && failureCode != "" {
			return DiscoveryOutcome{}, errors.New("invalid failure code")
		}
		_, err = r.DB.Exec(ctx, `INSERT INTO program_discovery_coverage
			(run_id,scope_kind,scope_key,scope_name,provider_role,source_id,status,checked_at,failure_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, runID, s.Kind, s.Key, s.Name, nullableText(s.ProviderRole), sourceID, status, checkedAt, nullableText(failureCode))
		if err != nil {
			return DiscoveryOutcome{}, err
		}
	}
	_, err = r.DB.Exec(ctx, `UPDATE program_discovery_runs SET state=$2,finished_at=now() WHERE id=$1`, runID, state)
	return DiscoveryOutcome{RunID: runID, State: state}, err
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
