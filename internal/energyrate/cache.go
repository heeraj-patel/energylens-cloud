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

// Cache stores the last EIA rate read per region (state code or "US"). It is
// keyed only by region — never by postal code, hub, or client — so it holds
// public aggregate data and nothing that links a location to a household.
type Cache interface {
	// Get returns the cached rate for region; ok=false on a miss.
	Get(ctx context.Context, region string) (r Rate, ok bool, err error)
	// Put upserts region's rate.
	Put(ctx context.Context, r Rate) error
}

// MemCache is the in-memory Cache used when no DSN is configured.
type MemCache struct {
	mu sync.RWMutex
	m  map[string]Rate
}

// NewMemCache returns an empty in-memory cache.
func NewMemCache() *MemCache { return &MemCache{m: map[string]Rate{}} }

// Get implements Cache.
func (c *MemCache) Get(_ context.Context, region string) (Rate, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[region]
	return r, ok, nil
}

// Put implements Cache.
func (c *MemCache) Put(_ context.Context, r Rate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[r.Region] = r
	return nil
}

// PgCache is the Postgres-backed Cache (table energy_rate_cache).
//
// The table is owned by migrations/0016_energy_rate_cache.sql, which mirrors
// kernel-common-contracts/db/cloud_schema.sql. The service does not create it:
// a second copy of the DDL here would be one more place to drift from the
// contract, and the runtime role should not need CREATE on the schema.
type PgCache struct {
	pool *pgxpool.Pool
}

// NewPgCache wraps pool after checking that energy_rate_cache exists (migration
// 0016 applied). A missing table is an error, so the caller falls back to the
// in-memory cache instead of failing every request.
func NewPgCache(ctx context.Context, pool *pgxpool.Pool) (*PgCache, error) {
	if _, err := pool.Exec(ctx, `SELECT 1 FROM energy_rate_cache LIMIT 0`); err != nil {
		return nil, fmt.Errorf("energyrate: energy_rate_cache not available (apply migrations/0016_energy_rate_cache.sql): %w", err)
	}
	return &PgCache{pool: pool}, nil
}

// Get implements Cache.
func (c *PgCache) Get(ctx context.Context, region string) (Rate, bool, error) {
	const q = `SELECT rate_usd_per_kwh::float8, period, fetched_at FROM energy_rate_cache WHERE region = $1`
	r := Rate{Region: region}
	err := c.pool.QueryRow(ctx, q, region).Scan(&r.USDPerKWh, &r.Period, &r.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, false, nil
	}
	if err != nil {
		return Rate{}, false, err
	}
	r.Period = r.Period.UTC()
	r.FetchedAt = r.FetchedAt.UTC()
	return r, true, nil
}

// Put implements Cache.
func (c *PgCache) Put(ctx context.Context, r Rate) error {
	const q = `
INSERT INTO energy_rate_cache (region, rate_usd_per_kwh, period, fetched_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (region) DO UPDATE SET
  rate_usd_per_kwh = EXCLUDED.rate_usd_per_kwh,
  period           = EXCLUDED.period,
  fetched_at       = EXCLUDED.fetched_at`
	fetched := r.FetchedAt
	if fetched.IsZero() {
		fetched = time.Now().UTC()
	}
	_, err := c.pool.Exec(ctx, q, r.Region, r.USDPerKWh, r.Period, fetched)
	return err
}
