# EnergyLens Cloud

One Go binary serves path-routed API modules. `GET /v1/energy/rates` remains public. With Postgres and OIDC configured, the API also serves `GET /v1/me` and owner-scoped `GET/POST /v1/homes`, `GET/PATCH /v1/homes/{home}`. `GET /healthz` and `GET /readyz` are probes. Inventory, hub claim, certificate, and sync operations are not registered while their trust protocols are still being built.

## Roles and listeners

`go run ./cmd/energylens-cloud --role=api` serves API routes. `--role=worker` serves probes and can run the opt-in metered rollup job. `--role=all` combines them for development. The default role is `all`, or `ENERGYLENS_CLOUD_ROLE` when set. Production refuses `all`. `ENERGYLENS_CLOUD_ENV` is `dev` by default; set `staging` or `production` explicitly in deployment.

The main API binds to `ENERGYLENS_CLOUD_BIND` (default `127.0.0.1:8091`) and is intended to sit behind a trusted HTTPS edge. It strips client-supplied identity headers. The optional, distinct hub listener binds to `ENERGYLENS_CLOUD_HUB_BIND` and requires `ENERGYLENS_CLOUD_HUB_TLS_CERT`, `ENERGYLENS_CLOUD_HUB_TLS_KEY`, and `ENERGYLENS_CLOUD_HUB_CLIENT_CA`. It verifies client certificates in the TLS handshake and currently serves probes only. Do not route hub sync traffic to it until certificate revocation and ownership epoch checks are implemented.

API and worker roles open one shared Postgres pool when `ENERGYLENS_CLOUD_DATABASE_URL` is set. Staging and production require the database URL and certificate-verified TLS (`sslmode=verify-full` with a trusted CA). Startup checks the database and the existing `energy_rate_cache` migration; `/readyz` checks database reachability. Development without a database keeps the existing in-memory rate cache. Set `EIA_API_KEY` for live EIA lookup. The dated July 2026 Texas published average remains the fallback for the phase-1 pilot; it is not a household tariff.

`ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED=true` explicitly enables bounded hourly/daily recomputation and guarded retention on the `worker` role (or development `all`) with a database. It is false by default. A pass starts immediately and repeats every minute, with a 30-second deadline and bounded batch/bucket/purge counts. Validate migration 0031 and the rollup/retention behavior against the deployment database before enabling it; no live deployment has done so yet.

Account routes require `ENERGYLENS_CLOUD_OIDC_ISSUER`, `ENERGYLENS_CLOUD_OIDC_AUDIENCE`, and `ENERGYLENS_CLOUD_OIDC_JWKS_URL` together. Production API startup refuses missing OIDC settings; staging and production require HTTPS issuer and JWKS URLs. Tokens must carry a valid RS256 signature, issuer, audience, expiry, and subject. Accounts are keyed by `(issuer, subject)`; verified email is mutable profile data. There is no authentication bypass. Home mutations require a UUID `Idempotency-Key`; PATCH also requires a strong numeric version `If-Match` ETag.

## Migrations

Run `ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL=... energylens-cloud migrate` with a separate DDL-capable migration role before starting API or worker processes. The binary embeds the existing 0016 rate-cache and 0017 reviewed-rating migrations plus additive account, inventory, telemetry, rollup and program-registry migrations through 0032. It serializes migration runs, records SQL checksums in `energylens_schema_migrations`, and rejects changed files. It never runs DDL during serving. A preexisting database without a ledger is safely baselined by reapplying the idempotent 0016/0017 migrations and recording them.

`go test ./...` covers rate behavior, the rating store, OIDC verification, account routes, role policy, listener isolation, header stripping, and migration file IDs. Set `ENERGYLENS_CLOUD_TEST_DATABASE_URL` to run the account fresh/upgrade, tenancy, and rating-store Postgres integration tests. The runtime has not been deployed or tested against a production RDS instance.

First-party auth remains disabled by default. The [local auth foundation](internal/account/LOCAL_AUTH_FOUNDATION.md) documents the file-backed key gate, a database-free `energylens-cloud check-local-auth-keys` operator check, and the signing key rotation sequence. The check validates proposed material; it does not enable routes or change a deployment.

## Hub device trust and ownership

Hub keys are registered only through an operator action using the dedicated migration/provisioning PostgreSQL role. There is no HTTP key-registration endpoint. After applying migrations, run:

```sh
ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL='postgres://...' energylens-cloud provision-hub \
  --local-hub-id elhub_example --public-key '<unpadded-base64url-Ed25519-key>' --approved-by '<administrator-id>'
```

The command returns the cloud hub UUID and a one-use enrollment nonce. Deliver both to the hub operator over an approved provisioning channel. The nonce expires after 15 minutes; `issue-hub-enroll-nonce --hub-id <uuid>` issues another for an already registered, nonrevoked hub. Production provisioning uses certificate-verified PostgreSQL TLS. Configure distinct migration/provisioning and API database roles; the command refuses the API role when both DSNs are configured. Keep stdout with the nonce out of routine logs.

The main API listener accepts signed enrollment requests only for registered keys and issued nonces. It signs a 24-hour client certificate using `ENERGYLENS_CLOUD_HUB_SIGN_CERT` and `ENERGYLENS_CLOUD_HUB_SIGN_KEY`; the signing CA must chain to `ENERGYLENS_CLOUD_HUB_CLIENT_CA`. Production API startup also requires a separate `ENERGYLENS_CLOUD_HUB_BIND` listener with its TLS certificate and key. Only that verified client-certificate listener accepts certificate renewal and `GET /v1/hubs/{hub}/ownership`. The renewal route checks the stored serial, revocation, expiry, hub identity SAN and ownership epoch, then revokes the prior certificate when issuing the new one. The ownership route returns the claimed home only to a certificate at the current epoch; an old certificate may renew once but cannot read the new home binding. After a verified hub peer is identified, a revoked or stale certificate yields `403 ownership_revoked`, while an unbound or tombstoned home/account yields `404 ownership_unbound`. The hub uses only these explicit codes to purge old-owner staging. A TLS rejection has no HTTP body, so the hub retains its bounded local lease until expiry.

An authenticated account owner begins a claim for one of their homes. The hub's authenticated local pairing channel issues a random possession proof and signs the challenge with its registered Ed25519 key. Cloud verifies the signature, proof hash, owner and expiry before atomically consuming the challenge and assigning the hub. The first claim advances epoch 1 to 2; unclaim advances the epoch again and revokes certificates. A newly claimed hub can use its still valid preclaim certificate once to renew at the new epoch. Claim, finish and unclaim mutations require UUID `Idempotency-Key` headers, and unclaim requires the current strong version `If-Match` value.

## Reviewed KB import

After migration 0021, an operator can import a KB publication signed by the pinned Ed25519 key:

```sh
ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL='postgres://...' \
ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE=/secure/path/reviewed-facts-public.pem \
energylens-cloud import-reviewed-catalog --artifact /secure/path/reviewed-facts.json --imported-by '<administrator-id>'
```

The command verifies canonical JSON, payload digest, key identity and signature before writing. It accepts a byte-identical replay, rejects conflicting product or fact IDs, and imports all rows in one transaction. The artifact supplies `reviewed_at` and source hashes but no source retrieval time, so imported `source_fetched_at` stays null. This command has no public HTTP route; imported facts are not yet distributed to customers.

Migration 0033 adds a separate immutable import for reviewed per-cycle and per-hour usage facts. With the same dedicated operator database role and pinned KB public key, run `energylens-cloud import-reviewed-usage-facts --artifact /secure/path/usage-facts.json --imported-by '<administrator-id>'`. The signed payload uses domain `energylens.reviewed-usage-facts.v1` and records exact or documented family scope, units, included energy, source and review hashes. This import does not modify annual catalog facts, expose a customer route, or feed useful-cost estimates yet.

## Appliance inventory

After migration 0022, authenticated owners can list, create, read and patch appliances under `/v1/homes/{home}/appliances`. Mutations use UUID `Idempotency-Key` headers; PATCH also uses the current strong version `If-Match` ETag. Each version records an immutable appliance snapshot. A catalog product ID is accepted only with a confirmed exact identity whose category, model, variant, region and fuel match that imported product. Archive, sensors, answers and estimates are separate later routes.

Migration 0041 adds a private, home-scoped candidate scan outbox. Saving a confirmed exact make/model with no linked reviewed catalog product queues one scan per normalized model per home; unresolved or edited names do not queue. The worker role polls pending requests every 10 seconds when both `ENERGYLENS_CLOUD_KB_PYTHON` and `ENERGYLENS_CLOUD_KB_SOURCES` point to an installed KB Python and reviewed source directory. `ENERGYLENS_CLOUD_KB_CANDIDATE_INDEX` may point to a recorded public candidate index. Default operation is offline (`scan-energy --skip-epa`); `ENERGYLENS_CLOUD_KB_ALLOW_EPA=true` enables the KB's bounded official EPA lookup only after source terms and network policy review. Each pass claims at most 10 models; at 03:00 UTC only, one pass also retries due missing-source rows after 24 hours, up to five attempts. Scanner/provider failures retry with bounded backoff outside that missing-source schedule. Reports remain private review candidates and never enter the customer catalog automatically. Physical home purge deletes the requests and their reports. The worker is disabled when KB paths are absent.

Program discovery refresh (migration 0051): authenticated clients can POST
`/v1/homes/{home}/programs/discovery-refresh` with `{"appliance_id":"<uuid>"}`
and a UUID `Idempotency-Key` header, then GET the returned request ID. The
private request snapshots owner, home, appliance, category, and location revision;
status becomes unavailable when that scope changes. Requests for the same ZIP
and category share one location-only job. The programs page continues to show
the latest finished coverage while a new request is pending or running. Reviewed
program terms are still evaluated at read time and are not published by refresh.

The worker is off by default. Set `ENERGYLENS_CLOUD_PROGRAM_REFRESH_ENABLED=true`
on a `worker` role with `ENERGYLENS_CLOUD_DATABASE_URL`, absolute paths
`ENERGYLENS_CLOUD_OEDI_IOU_CSV` and `ENERGYLENS_CLOUD_OEDI_NON_IOU_CSV`, their
full-file `ENERGYLENS_CLOUD_OEDI_IOU_SHA256` and
`ENERGYLENS_CLOUD_OEDI_NON_IOU_SHA256`, and a UTC
`ENERGYLENS_CLOUD_OEDI_RETRIEVED_AT` RFC3339 timestamp. Startup validates both
full operator CSVs before accepting work. Each pass claims one job and has a
bounded runtime; failed or abandoned jobs become terminal. OEDI's likely ZIP
assignments are candidate leads only. They cannot establish address service,
source terms, program eligibility, or savings.
