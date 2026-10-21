# energylens-cloud

Go 1.24 service (module `energylens.local/cloud`): one binary, one TLS listener, path-routed `/v1` API
plus a worker role and operator subcommands. Postgres via pgx. Chi router.

## Layout
- `cmd/energylens-cloud/` main.go (flags, subcommands), operator tools (program/tariff import, deletion journal, unresolved-device help export).
- `internal/app/` config (`config.go`), module mounting + router (`app.go`), `/v1/auth/config` (`auth_capabilities.go`).
- `internal/account` (OIDC verifier, local auth, homes, prefs, deletion, dev auth), `hubtrust` (enroll, claim, certs, epochs),
  `sensors`/`attachments`/`telemetry`/`kasaruntime` (hub sync + owner reads), `meteredhistory|summary|export|rollup`,
  `householdenergy` (dashboard, trends), `catalog`, `usagefact*`, `ratedinput*`, `inventory`, `tariff`,
  `estimation`, `questions`, `faq`, `recommendation`, `outcomes`, `cyclecomparison`, `programs` (+OEDI, source probe),
  `reports`, `energyrate`, `energyrating`, `energycategories`, `weather`, `datawiz*`, `democost`, `devicestate`, `designfixture`, `helpexport`, `migrate`, `apierror`.
- `migrations/` embedded SQL 0016..0087 (0055 unused) (`embed.go`); `docs/cloud-api.md` route index; `runbooks/`; `scripts/` (zip timezone builder).
- `.env-example` documents every env var (template for `.env-local`).

## Build / test (verified 2026-10-16)
- `go build ./...` passes (needs module download on first run).
- `go test ./...` passes; OEDI ZIP tests read `internal/programs/testdata/`.
- Postgres integration tests skip unless `ENERGYLENS_CLOUD_TEST_DATABASE_URL` is set.
- Deploy build: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/energylens-cloud ./cmd/energylens-cloud`.

## Run
- Launcher (preferred): `../energylens-dev-bash-scripts/run-energylens-cloud.sh {migrate|api|worker} [envfile] [--check]`.
  Uses `go run` unless `ENERGYLENS_CLOUD_BIN` is set. Templates in `../energylens-dev-bash-scripts/templates/cloud-local-*.env.example`.
- Direct: `go run ./cmd/energylens-cloud --role=api|worker|all`; subcommands: `migrate`, `check-config`, `check-local-auth-keys`,
  `provision-hub`, `issue-hub-enroll-nonce`, `import-reviewed-program`,
  `import-official-tariff`, `import-demo-cost-snapshot`, `export-datawiz-requests`, `read-datawiz-current`, `export-energy-categories`,
  `adopt-dev-account-username`, `unlock-dev-username-login`, `deletion-journal`, `export-unresolved-device-help`.
- Migrate with a separate DDL role (`ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL`) before serving; serving never runs DDL.
- Geekom deploy: `../energylens-common/scripts/deploy-geekom.sh cloud` cross-builds, copies to
  `~/energylens-pilot/bin/energylens-cloud-api`, restarts user units `energylens-cloud-api.service` and `energylens-cloud-worker.service`. Needs `ENERGYLENS_GEEKOM_SSH`.

## Config (see `.env-example`; validation in `internal/app/config.go`)
- `ENERGYLENS_CLOUD_ENV` dev (default) | staging | production. `RUNTIME_ENV` local | dev | test | staging | production;
  must agree with `ENERGYLENS_CLOUD_ENV` (`local` implies dev + local auth). Other values fail startup.
- `ENERGYLENS_CLOUD_ROLE` / `--role`: api | worker | all (default all; production refuses all).
- `ENERGYLENS_CLOUD_BIND` default `127.0.0.1:18092` (worker example 18092). `ENERGYLENS_CLOUD_HUB_BIND` is obsolete and fails startup.
- `ENERGYLENS_CLOUD_DATABASE_URL`; staging/production need `sslmode=verify-full`. `RUNTIME_ENV=local` needs a local/private PG host.
- TLS (API): `..._TLS_CERT`, `..._TLS_KEY`, `..._HUB_CLIENT_CA`, `..._HUB_SIGN_CERT`, `..._HUB_SIGN_KEY`;
  TLS 1.2+, client cert requested but optional (`VerifyClientCertIfGiven`); hub routes enforce it per handler.
- `..._METERED_EXPORT_CURSOR_KEY_FILE` (>=32 random bytes, 0600) required for owner API.
- Auth: OIDC (`..._OIDC_ISSUER`, `_AUDIENCE`, `_JWKS_URL`, RS256), local (`..._LOCAL_AUTH_*`: ENABLED, ACTIVE_KEY_ID,
  PRIVATE_KEY_FILE, PUBLIC_KEY_FILES JSON, AUDIENCE, RUNTIME_LOCAL_AUTH_AUDIENCE, LINK_ORIGIN, *_RATE_KEY_FILE, MAIL_ENABLED,
  RESEND_API_KEY/FROM, TRUSTED_PROXY_CIDRS), legacy `..._DEV_AUTH_*`. Production API needs OIDC or enabled local auth.
- Optional jobs/features all default off: METERED_ROLLUP, PROGRAM_REFRESH (+OEDI CSV/SHA vars), PROGRAM_SOURCE_PROBE, REPORT_*,
  ACCOUNT_DELETION (non-prod), SAMPLE_FIXTURES (dev/test only, allowlists), DATAWIZ_REQUESTS_DIR, `EIA_API_KEY`.

## Auth modes
- `RUNTIME_ENV=local`: username/password. `/v1/auth/config` -> methods `["username_password"]`; no email, OIDC or mail allowed
  (conflicting settings fail startup). Needs distinct `..._RUNTIME_LOCAL_AUTH_AUDIENCE`, Ed25519 key files, register+login rate keys.
- Outside local: `LOCAL_AUTH_ENABLED` -> `email_password` (verification + recovery mail via Resend, link origin must be bare HTTPS origin);
  OIDC -> `oidc`; dev auth -> `email_password`. No methods configured -> 503.
- Access JWT 15 min; refresh token 43 chars, 30 days, rotated per refresh.
- No CORS or per-platform client allowlist exists in the Go code (grep found none); OIDC redirect URIs live at the IdP.

## API map
- Probes `/healthz`, `/readyz` (outside `/v1`). Public: `GET /v1/auth/config`, `GET /v1/energy/rates?country&postal_code`, `/v1/auth/local/*`.
- Bearer: `/v1/me`, `/me/preferences`, `/me/profile`, `/homes[/{home}]`, appliances, sensors, attachments, tariffs, questions,
  estimates, recommendations, programs (`GET /programs?appliance_id`, `POST /programs/discovery-refresh`), dashboard, energy-trends,
  metered-history|summaries|export, reports, catalog, usage-facts, hub-claims (`POST /homes/{home}/hub-claims`, `POST /hubs/claim`).
- Hub (mTLS or enroll proof): `/v1/hubs/{hub}/{enroll,certificate,ownership,sensors,attachments,sync/batches,kasa-runtime/snapshots,
  catalog/*,usage-facts,owner-authorizations}`.
- Mutations need UUID `Idempotency-Key`; versioned PATCH/DELETE need `If-Match`. Error body `{code,message,request_id,details}`.
- Canonical contract: `../energylens-common-contracts/api/cloud-openapi.yaml`; `docs/cloud-api.md` is the index.
- `device-energy` is documented but not mounted; `design-pages` is gated by sample-fixture flags.

## Conventions
- Each module exposes `Routes`/`HubRoutes`/`AccountRoutes` mounted in `app.go`; new route => update OpenAPI, regenerate
  route manifest in contracts repo, update `docs/cloud-api.md`.
- New migration: next number in `migrations/`, mirror in contracts `db/cloud_migrations/` and `db/cloud-migration-ledger.md`.
- Checksums are recorded; never edit an applied migration.
- Commits: Conventional Commits; no push.

## Gotchas
- Hub keys are provisioned only via CLI `provision-hub` (no HTTP); claim needs the hub's possession proof.
- No documented cert generation/CA flow in this repo; server cert must be trusted by the client and cover the host/IP SAN.
- Old kernel path references remain in code comments (`kernel-common-contracts`).
