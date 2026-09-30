# EnergyLens Cloud

Go service foundation for electricity-rate lookup. The development server binds to `127.0.0.1:8091` and exposes `GET /healthz` and `GET /v1/energy/rates?country=US&postal_code=78732`. Set `EIA_API_KEY` to fetch current EIA residential averages. For the phase-1 Texas pilot, the service can fall back to the published July 2026 Texas residential average of 15.88¢/kWh, with its date and [EIA Table 5.6.A](https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a) URL in the response. This is a state average, not the household tariff. Other ZIPs without an EIA key or cached rate return a temporary-unavailable error.

Set `ENERGYLENS_CLOUD_BIND=127.0.0.1:18091` when running beside Kernel services on the Geekom. The hub uses `ENERGYLENS_CLOUD_URL=http://127.0.0.1:18091` for regional rate lookups.

## Reference code

Copied and adapted from `../kernel-cloud` at commit `2912c371267a54591978f13a47e829bf8d4804fd`:

- `internal/energyrate/{service,eia,cache,zipstate}.go`
- `internal/apierror/apierror.go`
- `migrations/0016_energy_rate_cache.sql`

The rate service's JSON response uses the standard library, so it does not need Kernel's wider HTTP middleware. The copied package includes an optional Postgres cache type; this development entry point uses memory only. Production authentication, tariff storage, and household-specific rates remain to be designed.

`migrations/0017_energy_rating_catalog.sql` and `internal/energyrating` add a separate Postgres storage seam for candidate and reviewed device ratings. The schema keeps EPA exact, OEM exact, and OEM family evidence distinct, preserves source and review metadata, and prevents candidate writes from changing reviewed rows. This is internal storage only: the cloud service does not ingest KB candidates, expose a rating API, or distribute catalog entries to the hub. Apply and validate the migration in the intended Postgres environment before using it. Set `ENERGYLENS_CLOUD_TEST_DATABASE_URL` to run its isolated-schema integration test.

```sh
EIA_API_KEY=... go run ./cmd/energylens-cloud
```

Verify with `go test ./...`; the suite covers the rate service and rating-store behavior. The rating migration integration test skips when its Postgres test URL is not configured.
