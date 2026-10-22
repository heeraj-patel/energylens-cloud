// Package energyrate implements GET /v1/energy/rates: the residential
// electricity rate for a household location, for the hub's energy cost
// estimator (kernel-common/docs/plans/2026-09-22-energy-cost-estimator-plan.md
// §3.4, task 1.6; contract: kernel-common-contracts/api/cloud-openapi.yaml
// getEnergyRate).
//
// Resolution for country=US:
//
//  1. ZIP → state via the offline prefix table (zipstate.go).
//  2. state average: the stored EIA row (written by Datawiz load-energy-rates)
//     if within MaxSourceAge, else →
//  3. national average: the same step for EIA's "US" row, else →
//  4. the cited static national value from config (source "national"), else
//  5. 503 rate_temporarily_unavailable (only when the static value is
//     disabled) -- a transient failure, not "no rate exists".
//
// "Stored" means the Cache row or, when newer or the Cache is failing, the
// in-memory last-known-good value per region. Any other country → 404
// rate_unavailable (a definitive "no rate exists for this location"). The
// cache is keyed by region only; postal codes are never stored and never
// logged above debug level.
package energyrate

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"energylens.local/cloud/internal/apierror"
)

// Rate sources (cloud-openapi.yaml EnergyRateSource). The cloud never returns
// "user" and does not return "utility" in v1.
const (
	SourceState    = "state"
	SourceNational = "national"
)

// MinCacheTTL is the floor on the age at which a stored row is served as stale.
const MinCacheTTL = 24 * time.Hour

// MaxSourceAge bounds how long a stored EIA value may be used: Datawiz reloads
// quarterly (at most 92 days apart) plus its timer delay. Static fallbacks have no fetch time.
const MaxSourceAge = 93 * 24 * time.Hour

// DefaultCacheTTL is the age at which a stored row is served as stale.
const DefaultCacheTTL = MaxSourceAge

// cacheBackoff is how long the Cache is left alone after a read error; each
// region is served from the in-memory last-known-good value meanwhile.
const cacheBackoff = time.Minute

// StaticRate is the cited national average used when no stored row can
// supply one.
type StaticRate struct {
	USDPerKWh float64   // <= 0 disables the static fallback
	AsOf      time.Time // first day of the period the value describes
	SourceURL string
}

// Options configures a Service.
type Options struct {
	Research     *datawizfacts.Store
	Cache        Cache
	CacheTTL     time.Duration
	Static       StaticRate
	StaticStates map[string]StaticRate
	GasCache     GasCache
	GasStatic    GasStaticRate
	Now          func() time.Time
	Logger       *slog.Logger
}

// Service resolves rates and serves the HTTP route.
type Service struct {
	research     *datawizfacts.Store
	cache        Cache
	ttl          time.Duration
	static       StaticRate
	staticStates map[string]StaticRate
	gasCache     GasCache
	gasStatic    GasStaticRate
	now          func() time.Time
	log          *slog.Logger

	mu             sync.Mutex
	lastGood       map[string]Rate // region → newest rate read from the cache
	cacheDownUntil time.Time       // no Cache.Get before this instant (after a cache error)
}

// NewService builds the service; zero-valued Options fields get defaults.
func NewService(o Options) *Service {
	if o.Cache == nil {
		o.Cache = NewMemCache()
	}
	if o.GasCache == nil {
		o.GasCache = NewMemGasCache()
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = DefaultCacheTTL
	}
	if o.CacheTTL < MinCacheTTL {
		o.CacheTTL = MinCacheTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{
		research:     o.Research,
		cache:        o.Cache,
		ttl:          o.CacheTTL,
		static:       o.Static,
		staticStates: o.StaticStates,
		gasCache:     o.GasCache,
		gasStatic:    o.GasStatic,
		now:          o.Now,
		log:          o.Logger,
		lastGood:     map[string]Rate{},
	}
}

// ErrUnavailable means no rate exists for this location at all (contract 404
// rate_unavailable) -- a definitive answer, returned only for a country no
// rate source covers (non-US in v1). Never returned for a US ZIP.
var ErrUnavailable = errors.New("energyrate: rate unavailable")

// ErrTemporarilyUnavailable means a US location for which no rate could be
// produced right now: nothing is stored for the state or the nation, and the
// static national value is disabled (contract
// 503 rate_temporarily_unavailable, retryable). A transient failure, not "no
// rate exists".
var ErrTemporarilyUnavailable = errors.New("energyrate: rate temporarily unavailable")

// Resolved is the response body (cloud-openapi.yaml EnergyRate).
type Resolved struct {
	ResearchFacts []datawizfacts.Fact `json:"research_facts,omitempty"`
	RateUSDPerKWh float64             `json:"rate_usd_per_kwh"`
	Source        string              `json:"source"`
	SourceLabel   string              `json:"source_label"`
	AsOf          string              `json:"as_of"`
	Region        *string             `json:"region"`
	SourceURL     string              `json:"source_url,omitempty"`
	FetchedAt     *time.Time          `json:"fetched_at"`
	IsStale       bool                `json:"is_stale"`
	IsStatic      bool                `json:"is_static"`
}

// Resolve runs the §3.4 fallback chain. country must be an upper-case ISO
// alpha-2 code and zip a normalized 5-digit ZIP (the handler validates both).
func (s *Service) Resolve(ctx context.Context, country, zip string) (Resolved, error) {
	resolved, err := s.resolveBase(ctx, country, zip)
	if err != nil || s.research == nil || country != "US" {
		return resolved, err
	}
	facts, err := s.research.Current(ctx, "zip", "zip:"+zip)
	if err != nil {
		return Resolved{}, err
	}
	for _, f := range facts {
		if f.Field == "rate" || f.Field == "provider_candidate" || f.Field == "jurisdiction" {
			resolved.ResearchFacts = append(resolved.ResearchFacts, f)
		}
	}
	return resolved, nil
}

func (s *Service) resolveBase(ctx context.Context, country, zip string) (Resolved, error) {
	if country != "US" {
		return Resolved{}, ErrUnavailable
	}
	if st := StateForZIP(zip); st != "" {
		if r, stale, ok := s.regionRate(ctx, st); ok {
			region := st
			return Resolved{
				RateUSDPerKWh: r.USDPerKWh,
				Source:        SourceState,
				SourceLabel:   stateNames[st] + " average (EIA)",
				AsOf:          r.Period.Format(time.DateOnly),
				Region:        &region,
				FetchedAt:     &r.FetchedAt,
				IsStale:       stale,
			}, nil
		}
		if fallback, ok := s.staticStates[st]; ok && fallback.USDPerKWh > 0 {
			region := st
			return Resolved{
				RateUSDPerKWh: fallback.USDPerKWh,
				Source:        SourceState,
				SourceLabel:   stateNames[st] + " average (EIA, published table)",
				AsOf:          fallback.AsOf.Format(time.DateOnly),
				Region:        &region,
				SourceURL:     fallback.SourceURL,
				IsStatic:      true,
			}, nil
		}
	} else {
		// Unknown prefix / territory / military: national, per the contract.
		// The ZIP itself stays out of logs above debug level.
		s.log.Debug("energyrate: ZIP prefix has no state mapping; using national average", "zip", zip)
	}
	if r, stale, ok := s.regionRate(ctx, NationalRegion); ok {
		resolved := national(r.USDPerKWh, r.Period)
		resolved.FetchedAt = &r.FetchedAt
		resolved.IsStale = stale
		return resolved, nil
	}
	if s.static.USDPerKWh > 0 {
		resolved := national(s.static.USDPerKWh, s.static.AsOf)
		resolved.SourceURL = s.static.SourceURL
		resolved.IsStatic = true
		return resolved, nil
	}
	return Resolved{}, ErrTemporarilyUnavailable
}

// NationalUSDPerKWh is the US average residential rate used for homes without
// a tariff: the stored EIA national row, else the static value.
func (s *Service) NationalUSDPerKWh(ctx context.Context) (float64, error) {
	r, err := s.resolveBase(ctx, "US", "")
	return r.RateUSDPerKWh, err
}

func national(rate float64, asOf time.Time) Resolved {
	return Resolved{
		RateUSDPerKWh: rate,
		Source:        SourceNational,
		SourceLabel:   "U.S. average (EIA)",
		AsOf:          asOf.Format(time.DateOnly),
		Region:        nil,
	}
}

// regionRate returns the stored rate within the source-age ceiling, stale once
// older than the TTL. Static fallbacks are handled by Resolve.
func (s *Service) regionRate(ctx context.Context, region string) (Rate, bool, bool) {
	cached, found := s.cachedRate(ctx, region)
	age := s.now().Sub(cached.FetchedAt)
	valid := found && !cached.FetchedAt.IsZero() && age >= 0 && age <= MaxSourceAge
	return cached, valid && age >= s.ttl, valid
}

// cachedRate returns the newer of the Cache's row and the in-memory
// last-known-good value for region, skipping the Cache while it is backed off
// after an error. Whatever it returns becomes the new last-known-good value.
func (s *Service) cachedRate(ctx context.Context, region string) (Rate, bool) {
	var best Rate
	var found bool
	if s.cacheUsable() {
		r, ok, err := s.cache.Get(ctx, region)
		switch {
		case err != nil:
			s.log.Warn("energyrate: cache read failed; serving in-memory rates", "region", region, "err", err)
			s.markCacheDown()
		case ok:
			best, found = r, true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if lg, ok := s.lastGood[region]; ok && (!found || lg.FetchedAt.After(best.FetchedAt)) {
		best, found = lg, true
	}
	if found {
		s.lastGood[region] = best
	}
	return best, found
}

func (s *Service) cacheUsable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.now().Before(s.cacheDownUntil)
}

func (s *Service) markCacheDown() {
	s.mu.Lock()
	s.cacheDownUntil = s.now().Add(cacheBackoff)
	s.mu.Unlock()
}

// Routes registers GET /energy/rates relative to r (the caller mounts r under
// /v1, so it serves as GET /v1/energy/rates). This local development server
// binds to loopback; production access control must be added before deployment.
func (s *Service) Routes(r chi.Router) {
	r.Get("/energy/rates", s.handleRates)
}

var (
	countryRe = regexp.MustCompile(`^[A-Z]{2}$`)
	usZIPRe   = regexp.MustCompile(`^([0-9]{5})(?:-?[0-9]{4})?$`)
)

// handleRates implements GET /v1/energy/rates?country=&postal_code=.
func (s *Service) handleRates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	country := q.Get("country")
	postal := strings.TrimSpace(q.Get("postal_code"))
	if country == "" || postal == "" {
		apierror.Write(w, apierror.BadRequest("missing_parameter", "country and postal_code are required"))
		return
	}
	if !countryRe.MatchString(country) {
		apierror.Write(w, apierror.BadRequest("invalid_country", "country must be an ISO 3166-1 alpha-2 code (e.g. US)"))
		return
	}
	if country != "US" {
		apierror.Write(w, rateUnavailable())
		return
	}
	m := usZIPRe.FindStringSubmatch(postal)
	if m == nil {
		apierror.Write(w, apierror.BadRequest("invalid_postal_code", "postal_code must be a 5-digit US ZIP (ZIP+4 accepted)"))
		return
	}
	res, err := s.Resolve(r.Context(), country, m[1])
	if errors.Is(err, ErrUnavailable) {
		apierror.Write(w, rateUnavailable())
		return
	}
	if errors.Is(err, ErrTemporarilyUnavailable) {
		apierror.Write(w, rateTemporarilyUnavailable())
		return
	}
	if err != nil {
		apierror.Write(w, apierror.Internal("rate lookup failed"))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

func rateUnavailable() *apierror.APIError {
	return apierror.NotFound("rate_unavailable", "no electricity rate source covers this location")
}

// rateTemporarilyUnavailable is the contract's 503 for a US location whose
// rate could not be produced right now (retryable: true), distinct from the
// 404 rateUnavailable used only when no rate source exists for the location
// at all (non-US).
func rateTemporarilyUnavailable() *apierror.APIError {
	return apierror.New(http.StatusServiceUnavailable, "rate_temporarily_unavailable",
		"no electricity rate could be resolved right now; try again later", true)
}
