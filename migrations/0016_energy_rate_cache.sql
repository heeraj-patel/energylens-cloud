-- 0016_energy_rate_cache.sql — server-side cache for GET /v1/energy/rates.
--
-- Derived from kernel-common-contracts/db/cloud_schema.sql (SOURCE OF TRUTH),
-- energy_rate_cache (contracts commit 1a0a893). This migration is the only
-- thing that creates the table: kb-cdn-api checks it exists at startup and
-- falls back to an in-memory cache with a WARN when it is missing.
--
-- One row per region: a US state code or 'US' (national), holding the latest
-- EIA residential average retail price. Keyed by region only — no postal
-- code, hub, or client identifier is ever stored (public aggregate data).
-- Rows older than the service's cache TTL (KERNEL_ENERGY_RATE_CACHE_TTL,
-- default 720h, minimum 24h) are refreshed from EIA on the next request, and
-- still served as a stale fallback when EIA is unreachable or no key is set.

CREATE TABLE IF NOT EXISTS energy_rate_cache (
  region           TEXT PRIMARY KEY CHECK (region ~ '^[A-Z]{2}$'),
  rate_usd_per_kwh NUMERIC NOT NULL CHECK (rate_usd_per_kwh > 0),
  period           DATE NOT NULL,                 -- first day of the EIA data month
  fetched_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
