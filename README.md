# EnergyLens Cloud

One Go binary serves path-routed API modules on a single TLS listener. `GET /v1/energy/rates` remains public. With Postgres and OIDC configured, the API serves authenticated account and home routes. Hub enrollment and owner claim routes use their existing proof and ownership checks; Hub-only ownership, sync, telemetry, attachment, runtime, catalog and usage-fact routes additionally require a verified client certificate. `GET /healthz` and `GET /readyz` are probes.

## Roles and listeners

`go run ./cmd/energylens-cloud --role=api` serves API routes. `--role=worker` serves probes and can run the opt-in metered rollup job. `--role=all` combines them for development. The default role is `all`, or `ENERGYLENS_CLOUD_ROLE` when set. Production refuses `all`. `ENERGYLENS_CLOUD_ENV` is `dev` by default; set `staging` or `production` explicitly in deployment.

The Cloud listener binds to `ENERGYLENS_CLOUD_BIND` (default `127.0.0.1:18091`) and serves owner and Hub routes on the same TLS listener. It requires `ENERGYLENS_CLOUD_TLS_CERT`, `ENERGYLENS_CLOUD_TLS_KEY`, and `ENERGYLENS_CLOUD_HUB_CLIENT_CA`; TLS requests a client certificate and verifies it when supplied. Hub-only handlers also require a verified certificate and retain their durable revocation and ownership-epoch checks. Owner routes require OIDC bearer authentication (or the explicitly enabled local auth verifier). The listener strips client-supplied identity headers. `ENERGYLENS_CLOUD_HUB_BIND` is obsolete and causes startup validation to fail; do not configure a second Cloud listener.

API and worker roles open one shared Postgres pool when `ENERGYLENS_CLOUD_DATABASE_URL` is set. Staging and production require the database URL and certificate-verified TLS (`sslmode=verify-full` with a trusted CA). Startup checks the database and the existing `energy_rate_cache` migration; `/readyz` checks database reachability. Development without a database keeps the existing in-memory rate cache. Set `EIA_API_KEY` for live EIA lookup. The dated July 2026 Texas published average remains the fallback for the phase-1 pilot; it is not a household tariff.

`ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED=true` explicitly enables bounded hourly/daily recomputation and guarded retention on the `worker` role (or development `all`) with a database. It is false by default. A pass starts immediately and repeats every minute, with a 30-second deadline and bounded batch/bucket/purge counts. Validate migration 0031 and the rollup/retention behavior against the deployment database before enabling it; no live deployment has done so yet.

OIDC account routes require `ENERGYLENS_CLOUD_OIDC_ISSUER`, `ENERGYLENS_CLOUD_OIDC_AUDIENCE`, and `ENERGYLENS_CLOUD_OIDC_JWKS_URL` together. Production API startup requires OIDC or explicitly enabled EnergyLens local authentication; staging and production OIDC settings require HTTPS issuer and JWKS URLs. OIDC tokens must carry a valid RS256 signature, issuer, audience, expiry, and subject. Accounts are keyed by `(issuer, subject)`; verified email is mutable profile data. There is no authentication bypass. Home mutations require a UUID `Idempotency-Key`; PATCH also requires a strong numeric version `If-Match` ETag.

Owner API deployments also require `ENERGYLENS_CLOUD_METERED_EXPORT_CURSOR_KEY_FILE` for metered-export pagination. Generate a dedicated key file with at least 32 cryptographically random bytes (for example, `umask 077; head -c 32 /dev/urandom > /secure/path/metered-export-cursor.key`), mount the same persistent file on every API replica, and keep it across restarts. The file must be private (`0600` or stricter) and cannot share a path with the hub or local-auth signing key. Startup fails if an owner API route is enabled without a valid key. For rotation, set the new active file and `ENERGYLENS_CLOUD_METERED_EXPORT_PREVIOUS_CURSOR_KEY_FILE` to the old file on every replica; remove the previous file setting once old cursors no longer need to work. Unsigned cursors issued before this change are rejected, so clients must restart pagination.

## Migrations

Run `ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL=... energylens-cloud migrate` with a separate DDL-capable migration role before starting API or worker processes. The binary embeds migrations through 0064, including the original rate-cache and reviewed-rating migrations, account, inventory, telemetry, rollup and program-registry schema, reviewed annual and usage-fact revision lifecycles, catalog generations, and the nullable imported demo cost snapshot on homes. Apply 0064 before starting a binary that reads `homes.demo_cost_snapshot`, including for homes without an imported snapshot. It serializes migration runs, records SQL checksums in `energylens_schema_migrations`, and rejects changed files. It never runs DDL during serving. A preexisting database without a ledger is safely baselined by reapplying the idempotent 0016/0017 migrations and recording them.

`go test ./...` covers rate behavior, OIDC and account routes, role policy, same-listener TLS access, header stripping, signed annual and usage-fact import lifecycles, active owner/Hub usage-fact reads, catalog response generation and digests, and migration file IDs. PostgreSQL integration tests use `ENERGYLENS_CLOUD_TEST_DATABASE_URL` where configured and otherwise skip; set it to run database-backed migration, import, and API tests. No production deployment or production RDS validation is documented here.

First-party auth remains disabled by default. The [local auth foundation](internal/account/LOCAL_AUTH_FOUNDATION.md) documents the file-backed key gate, a database-free `energylens-cloud check-local-auth-keys` operator check, and the signing key rotation sequence. The check validates proposed material; it does not enable routes or change a deployment.

## Hub device trust and ownership

Hub keys are registered only through an operator action using the dedicated migration/provisioning PostgreSQL role. There is no HTTP key-registration endpoint. After applying migrations, run:

```sh
ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL='postgres://...' energylens-cloud provision-hub \
  --local-hub-id elhub_example --public-key '<unpadded-base64url-Ed25519-key>' --approved-by '<administrator-id>'
```

The command returns the cloud hub UUID and a one-use enrollment nonce. Deliver both to the hub operator over an approved provisioning channel. The nonce expires after 15 minutes; `issue-hub-enroll-nonce --hub-id <uuid>` issues another for an already registered, nonrevoked hub. Production provisioning uses certificate-verified PostgreSQL TLS. Configure distinct migration/provisioning and API database roles; the command refuses the API role when both DSNs are configured. Keep stdout with the nonce out of routine logs.

The Cloud listener accepts signed enrollment requests only for registered keys and issued nonces. It signs a 24-hour client certificate using `ENERGYLENS_CLOUD_HUB_SIGN_CERT` and `ENERGYLENS_CLOUD_HUB_SIGN_KEY`; the signing CA must chain to `ENERGYLENS_CLOUD_HUB_CLIENT_CA`. The same listener accepts certificate renewal and `GET /v1/hubs/{hub}/ownership`, with verified-client-certificate middleware on Hub-only handlers. The renewal route checks the stored serial, revocation, expiry, hub identity SAN and ownership epoch, then revokes the prior certificate when issuing the new one. The ownership route returns the claimed home only to a certificate at the current epoch; an old certificate may renew once but cannot read the new home binding. After a verified hub peer is identified, a revoked or stale certificate yields `403 ownership_revoked`, while an unbound or tombstoned home/account yields `404 ownership_unbound`. The hub uses only these explicit codes to purge old-owner staging. A TLS rejection has no HTTP body, so the hub retains its bounded local lease until expiry.

An authenticated account owner begins a claim for one of their homes. The hub's authenticated local pairing channel issues a random possession proof and signs the challenge with its registered Ed25519 key. Cloud verifies the signature, proof hash, owner and expiry before atomically consuming the challenge and assigning the hub. The first claim advances epoch 1 to 2; unclaim advances the epoch again and revokes certificates. A newly claimed hub can use its still valid preclaim certificate once to renew at the new epoch. Claim, finish and unclaim mutations require UUID `Idempotency-Key` headers, and unclaim requires the current strong version `If-Match` value.

## Reviewed KB import

After migration 0021, an operator can import a KB publication signed by the pinned Ed25519 key:

```sh
ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL='postgres://...' \
ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE=/secure/path/reviewed-facts-public.pem \
energylens-cloud import-reviewed-catalog --artifact /secure/path/reviewed-facts.json --imported-by '<administrator-id>'
```

The command verifies canonical JSON, payload digest, key identity and signature before writing. It accepts an identical replay, rejects conflicting product or fact IDs, and imports all rows in one transaction. The artifact supplies `reviewed_at` and source hashes but no source retrieval time, so imported `source_fetched_at` stays null. Import remains operator-only; read routes are described below.

Migration 0033 adds a separate import for reviewed per-cycle and per-hour usage facts. With the same dedicated operator database role and pinned KB public key, run `energylens-cloud import-reviewed-usage-facts --artifact /secure/path/usage-facts.json --imported-by '<administrator-id>'`. It verifies signed v1 publications and the v2 revision lifecycle, which supports superseding or retracting a prior fact while preserving publication and review provenance. The signed payload records exact or documented family scope, units, included energy, source and review hashes. This import is separate from annual catalog facts.

Signed annual catalog v2 publications support reviewed fact supersession and retraction through the existing `import-reviewed-catalog` operator command. Both import commands use `ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE`, verify canonical JSON, the pinned Ed25519 key, payload digest and signature, then persist the publication transactionally. A replay is idempotent; conflicting or invalid revisions are rejected.

## Reviewed catalog and usage reads

With the API role, database and owner authentication enabled, owners can read reviewed annual catalog products at `GET /v1/catalog/products/{product}` and search with `POST /v1/catalog/model-search`. Both responses include `X-Catalog-Generation`, `X-Catalog-Response-SHA256`, and a SHA-256 `ETag`; search also requires a UUID `Idempotency-Key`. Generation identifies the catalog publication snapshot and the response digest covers the returned JSON representation. Hubs can use the parallel `/v1/hubs/{hub}/catalog/...` routes with a current verified client certificate.

Active reviewed usage facts are available to authenticated owners at `GET /v1/usage-facts` and to a currently claimed Hub with a verified client certificate at `GET /v1/hubs/{hub}/usage-facts`. Both routes require an exact query scope: `category`, `manufacturer`, `model`, `fuel`, and `variant`. The response contains only active facts and their source/review provenance. Hub requests additionally validate certificate serial, revocation and ownership epoch against durable state. These reads do not calculate home estimates or savings.

## Appliance inventory

After migration 0022, authenticated owners can list, create, read and patch appliances under `/v1/homes/{home}/appliances`. Mutations use UUID `Idempotency-Key` headers; PATCH also uses the current strong version `If-Match` ETag. Each version records an immutable appliance snapshot. A catalog product ID is accepted only with a confirmed exact identity whose category, model, variant, region and fuel match that imported product. Archive, sensors, answers and estimates are separate later routes.

Migration 0041 adds a private, home-scoped candidate scan outbox. Saving a confirmed exact make/model with no linked reviewed catalog product queues one scan per normalized model per home; unresolved or edited names do not queue. The worker role polls pending requests every 10 seconds when both `ENERGYLENS_CLOUD_KB_PYTHON` and `ENERGYLENS_CLOUD_KB_SOURCES` point to an installed KB Python and reviewed source directory. `ENERGYLENS_CLOUD_KB_CANDIDATE_INDEX` may point to a recorded public candidate index. Default operation is offline (`scan-energy --skip-epa`); `ENERGYLENS_CLOUD_KB_ALLOW_EPA=true` enables the KB's bounded official EPA lookup only after source terms and network policy review. Each pass claims at most 10 models; at 03:00 UTC only, one pass also retries due missing-source rows after 24 hours, up to five attempts. Scanner/provider failures retry with bounded backoff outside that missing-source schedule. Reports remain private review candidates and never enter the customer catalog automatically. Physical home purge deletes the requests and their reports. The worker is disabled when KB paths are absent. Set `ENERGYLENS_CLOUD_KB_HELP_OUTPUT_PATH` to an absolute private server path ending in `unresolved_device_help.jsonl` to have the worker atomically refresh a 0600 human research queue after KB passes and at startup. The queue includes actual appliance IDs and model identities only, after five completed no-evidence attempts. An operator can refresh the same file with `energylens-cloud export-unresolved-device-help -output /private/path/unresolved_device_help.jsonl`.

Google CSE search is a separate opt-in for KB scans. After approving the CSE terms and confirming existing-customer eligibility, configure `ENERGYLENS_CLOUD_KB_GOOGLE_SEARCH=true`, `ENERGYLENS_CLOUD_KB_GOOGLE_SITE` to one public OEM hostname, `ENERGYLENS_CLOUD_KB_GOOGLE_EXISTING_CUSTOMER=true`, and `ENERGYLENS_CLOUD_KB_ALLOW_EPA=true`. Provide `ENERGYLENS_GOOGLE_CSE_API_KEY` and `ENERGYLENS_GOOGLE_CSE_ID` through the worker's secret environment. Startup rejects incomplete configuration; credentials are consumed by the KB subprocess environment and never added to its arguments or logs. Omit these settings to keep scans offline.

A waste-specific miss covers only the completed EPA model check and configured site search. It is not a claim that no source exists elsewhere. Search leads and standby ratings remain private review candidates; neither is published as a measured waste finding. The optional appliance-list miss result appears only after the scan reaches its terminal state. A verified device waste interval is still required before the hourly dummy rate can be allocated.

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

`ENERGYLENS_CLOUD_PROGRAM_SOURCE_PROBE_ENABLED=true` additionally permits the
refresh worker to check a registered, enabled, terms-approved HTTPS HTML/feed/API
page when a ZIP scope names its source key. The probe uses one public DNS answer
and a pinned connection, blocks redirects/private addresses, and caps the
response at the registered byte budget. A successful fetch stores immutable
source bytes and records source-check coverage. The bytes remain unreviewed;
the worker does not extract terms or publish offers. An operator may pass the
snapshot ID, exact bytes, and recorded metadata to `ImportReviewed` after a
separate human review. The import rejects a mismatch and keeps the reviewed
revision linked to that fetched snapshot. OEDI utility leads have no registered
source keys, so this flag alone does not expand ZIP provider or incentive
coverage.

The [program source review queue](internal/programs/source_review_queue.json) records
official page leads checked on 2026-10-03. It contains no approved source
registrations, ZIP coverage, reviewed terms, or publishable offers. An operator
must complete each listed access and terms review before creating an enabled
`program_sources` row; changing this file has no runtime effect.

# Isolated mobile design fixtures

The 14 page-specific mobile design baselines are embedded in the API binary and are unavailable by default. For an isolated development or test account and home, set `ENERGYLENS_CLOUD_SAMPLE_FIXTURES_ENABLED=true`, `ENERGYLENS_CLOUD_SAMPLE_FIXTURE_USER_IDS` to a comma-separated list of internal user UUIDs, and `ENERGYLENS_CLOUD_SAMPLE_FIXTURE_HOME_IDS` to a comma-separated list of home UUIDs. The API rejects activation in staging or production. Both allowlists and normal bearer authentication and home ownership are required for `GET /v1/homes/{home}/design-pages/{page}`. The route does not accept client-supplied sample selectors, and its response disables actions and caching. The embedded content remains an internal baseline pending owner approval for backend use; configuring the gate is the activation step after that approval.

Dashboard and energy-trends can read a persisted synthetic cost snapshot from `homes.demo_cost_snapshot`. An operator imports the approved schedule with `energylens-cloud import-demo-cost-snapshot --seed-mobile-demo-v1 --owner-id <user UUID> --home-id <home UUID>`; alternatively, a validated snapshot can be supplied on stdin. The command uses the migration database role and verifies owner/home/timezone scope transactionally. Request handlers read only the DB value; they do not load a packaged JSON or Markdown fixture or require a runtime allowlist. Synthetic rows use the 18 `demo-device-XX` identities, include nullable names, and report cost only: energy and unclassified cost stay null, with synthetic provenance. Measured interval data takes precedence and retains the live API's existing classification behavior.

`GET /v1/homes/{home}/energy-trends?granularity=day&window=trailing_30_days` selects 30 home-local dates from 29 days before today through today. `window=last_5_occurrences` supports `granularity=day|week|month`: it returns exactly five local days, Monday-start weeks, or calendar months, ending with the current occurrence and crossing month/year boundaries as needed. Daily buckets use the stored full-day model, including today's date; hourly buckets distribute modeled daily cost across local hours through `Now` and keep future hours unavailable. Weekly and monthly buckets aggregate modeled local days through today. Without `window`, existing calendar period selection is unchanged.

# Initial household energy reads

Authenticated `GET /v1/homes/{home}/dashboard` and `GET /v1/homes/{home}/energy-trends?granularity=hour|day|week|month` read corrected, owner-scoped energy intervals in a repeatable-read snapshot. The period uses the home's timezone and calendar boundaries; unsupported cost and useful/waste allocations stay null. Energy from a single unambiguous metering point is reported as a measured partial subtotal under `attributed_devices`, with unknown coverage. Missing intervals and overlapping distinct meters remain unavailable. Cross-bucket intervals are excluded rather than estimated. The device-energy read and stable pagination remain unimplemented.
