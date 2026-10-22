package energyrate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Gas rate bases (stored later as gas_rate_basis).
const (
	GasBasisState    = "state_average"
	GasBasisNational = "national_average"
	GasBasisStatic   = "static"
)

// GasRate is one region's mean residential gas price over the latest 12 months.
type GasRate struct {
	Region      string    // two-letter state code, or NationalRegion
	USDPerTherm float64   // mean of the months with data, converted from $/Mcf
	Period      time.Time // first day of the latest month with data (UTC)
	FetchedAt   time.Time
}

// GasStaticRate is the cited US value used when no stored row can supply one.
type GasStaticRate struct {
	USDPerTherm float64 // <= 0 disables the static fallback
	AsOf        time.Time
	SourceURL   string
}

// GasResolved is a resolved gas rate with its provenance.
type GasResolved struct {
	USDPerTherm float64
	Basis       string // GasBasis*
	Region      *string
	AsOf        string
	FetchedAt   *time.Time
	IsStale     bool
	SourceURL   string
}

// GasCache reads the stored gas rate per region (state code or "US").
type GasCache interface {
	Get(ctx context.Context, region string) (GasRate, bool, error)
}

// MemGasCache is the in-memory GasCache used when no DSN is configured.
type MemGasCache struct {
	mu sync.RWMutex
	m  map[string]GasRate
}

// NewMemGasCache returns an empty in-memory gas rate cache.
func NewMemGasCache() *MemGasCache { return &MemGasCache{m: map[string]GasRate{}} }

// Get implements GasCache.
func (c *MemGasCache) Get(_ context.Context, region string) (GasRate, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[region]
	return r, ok, nil
}

// Put stores region's gas rate.
func (c *MemGasCache) Put(r GasRate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[r.Region] = r
}

// PgGasCache reads gas_rate_cache (migration 0092), which Datawiz load-energy-rates writes.
type PgGasCache struct {
	pool *pgxpool.Pool
}

// NewPgGasCache wraps pool after checking that gas_rate_cache exists.
func NewPgGasCache(ctx context.Context, pool *pgxpool.Pool) (*PgGasCache, error) {
	if _, err := pool.Exec(ctx, `SELECT 1 FROM gas_rate_cache LIMIT 0`); err != nil {
		return nil, fmt.Errorf("energyrate: gas_rate_cache not available (apply migrations/0092_gas_rate_cache.sql): %w", err)
	}
	return &PgGasCache{pool: pool}, nil
}

// Get implements GasCache.
func (c *PgGasCache) Get(ctx context.Context, region string) (GasRate, bool, error) {
	const q = `SELECT rate_usd_per_therm::float8, period, fetched_at FROM gas_rate_cache WHERE region = $1`
	r := GasRate{Region: region}
	err := c.pool.QueryRow(ctx, q, region).Scan(&r.USDPerTherm, &r.Period, &r.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GasRate{}, false, nil
	}
	if err != nil {
		return GasRate{}, false, err
	}
	r.Period = r.Period.UTC()
	r.FetchedAt = r.FetchedAt.UTC()
	return r, true, nil
}

// ResolveGas returns the gas rate for a US ZIP ("" means national): the stored
// EIA state mean, else the US mean, else the static value, else ErrTemporarilyUnavailable.
func (s *Service) ResolveGas(ctx context.Context, zip string) (GasResolved, error) {
	if st := StateForZIP(zip); st != "" {
		if r, stale, ok := s.gasRegionRate(ctx, st); ok {
			region := st
			return GasResolved{USDPerTherm: r.USDPerTherm, Basis: GasBasisState, Region: &region,
				AsOf: r.Period.Format(time.DateOnly), FetchedAt: &r.FetchedAt, IsStale: stale}, nil
		}
	}
	if r, stale, ok := s.gasRegionRate(ctx, NationalRegion); ok {
		return GasResolved{USDPerTherm: r.USDPerTherm, Basis: GasBasisNational,
			AsOf: r.Period.Format(time.DateOnly), FetchedAt: &r.FetchedAt, IsStale: stale}, nil
	}
	if s.gasStatic.USDPerTherm > 0 {
		return GasResolved{USDPerTherm: s.gasStatic.USDPerTherm, Basis: GasBasisStatic,
			AsOf: s.gasStatic.AsOf.Format(time.DateOnly), SourceURL: s.gasStatic.SourceURL}, nil
	}
	return GasResolved{}, ErrTemporarilyUnavailable
}

// gasRegionRate mirrors regionRate: the stored row within MaxSourceAge, stale
// once older than the TTL. A read error is logged and treated as a miss.
func (s *Service) gasRegionRate(ctx context.Context, region string) (GasRate, bool, bool) {
	cached, found, err := s.gasCache.Get(ctx, region)
	if err != nil {
		s.log.Warn("energyrate: gas rate read failed", "region", region, "err", err)
		return GasRate{}, false, false
	}
	age := s.now().Sub(cached.FetchedAt)
	valid := found && !cached.FetchedAt.IsZero() && age >= 0 && age <= MaxSourceAge
	return cached, valid && age >= s.ttl, valid
}
