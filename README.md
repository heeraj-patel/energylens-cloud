# EnergyLens Cloud

Go service foundation for electricity-rate lookup. The development server binds to `127.0.0.1:8091` and exposes `GET /healthz` and `GET /v1/energy/rates?country=US&postal_code=60601`. Set `EIA_API_KEY` to fetch current EIA residential averages. With no key or cached rate, the endpoint returns a temporary-unavailable error; it does not invent a rate. A returned EIA state average is an estimate and is not a household utility tariff.

## Reference code

Copied and adapted from `../kernel-cloud` at commit `2912c371267a54591978f13a47e829bf8d4804fd`:

- `internal/energyrate/{service,eia,cache,zipstate}.go`
- `internal/apierror/apierror.go`
- `migrations/0016_energy_rate_cache.sql`

The rate service's JSON response now uses the standard library, so it does not need Kernel's wider HTTP middleware. The copied package still includes its optional Postgres cache type; this development entry point uses memory only. Production authentication, tariff storage, and household-specific rates remain to be designed.

```sh
EIA_API_KEY=... go run ./cmd/energylens-cloud
```

Verified locally with `go test ./...` (the initial copied package has no transferred test files yet).
