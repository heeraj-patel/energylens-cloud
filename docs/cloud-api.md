# EnergyLens Cloud API reference

This reference inventories the HTTP API mounted by `energylens-cloud`. The canonical [OpenAPI contract](../../energylens-common-contracts/api/cloud-openapi.yaml) defines complete JSON fields, parameters, headers, and error responses. This page indexes every operation and its request/success schemas.

## Runtime and security

- `internal/app/app.go` mounts API operations below `/v1`; `/healthz` and `/readyz` are outside that prefix. The listener uses TLS.
- API routes are built for `api` and `all` roles. Account routes require a database and a configured verifier. Feature flags can gate selected route groups.
- `bearer`: `Authorization: Bearer <access-token>`; identity is derived from token verification and home access is owner scoped.
- `hub-mtls`: verified TLS client certificate plus hub certificate/revocation and ownership checks. Caller-supplied identity headers are stripped.
- `hub-enroll`: bootstrap enrollment proof using the provisioned hub identity and nonce.
- `public`: no bearer token required; the listener remains TLS protected.
- Contract statuses `active`, `gated`, and `planned` describe availability; runtime gates are noted below.

## Operational probes

| Method | Path | Auth / status | Response |
|---|---|---|---|
| `GET` | `/healthz` | public / 200 | Plain text `ok`; process liveness only. |
| `GET` | `/readyz` | public / 200 or 503 | Plain text `ready` or `not ready`; checks database readiness when configured. |

## Route index

Paths below are relative to `/v1`. Schema names resolve to `components/schemas` in the OpenAPI file unless marked external. `—` means no JSON body. Exact status codes, fields, and operation-specific errors are in the linked contract.

### Account

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `POST` | `/auth/local/registration` | `requestLocalRegistration` | `public` / `gated` | — | [`LocalRegistrationRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalRegistrationRequest) or local username request | 201 [`LocalTokens`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalTokens) for new email account, 201 `LocalUsernameCreated` for local username account; 202 [`LocalAccepted`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalAccepted) for existing or throttled email | — |
| `POST` | `/auth/local/verify-email` | `verifyLocalEmail` | `public` / `gated` | — | [`LocalTokenRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalTokenRequest) | 204 — | — |
| `POST` | `/auth/local/login` | `loginLocalAccount` | `public` / `gated` | — | [`LocalRegistrationRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalRegistrationRequest) | 200 [`LocalTokens`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalTokens) | — |
| `POST` | `/auth/local/refresh` | `refreshLocalSession` | `public` / `gated` | — | [`LocalRefreshRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalRefreshRequest) | 200 [`LocalTokens`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalTokens) | — |
| `POST` | `/auth/local/logout` | `logoutLocalSession` | `public` / `gated` | — | [`LocalRefreshRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalRefreshRequest) | 204 — | — |
| `POST` | `/auth/local/recovery` | `requestLocalRecovery` | `public` / `gated` | — | [`LocalEmailRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalEmailRequest) | 202 [`LocalAccepted`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalAccepted) | — |
| `POST` | `/auth/local/reset-password` | `resetLocalPassword` | `public` / `gated` | — | [`LocalResetRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-LocalResetRequest) | 204 — | — |
| `GET` | `/me` | `getMe` | `bearer` / `active` | — | — | 200 [`Account`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Account) | — |
| `DELETE` | `/me` | `deleteMe` | `bearer` / `gated` | `header Idempotency-Key*` | — | 204 — | — |
| `GET` | `/me/preferences` | `getAccountPreferences` | `bearer` / `active` | — | — | 200 [`AccountPreferences`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AccountPreferences) | `ETag` |
| `PATCH` | `/me/preferences` | `updateAccountPreferences` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | [`AccountPreferencesPatch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AccountPreferencesPatch) | 200 [`AccountPreferences`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AccountPreferences) | `ETag` |

### Homes

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes` | `listHomes` | `bearer` / `active` | — | — | 200 object | — |
| `POST` | `/homes` | `createHome` | `bearer` / `active` | `header Idempotency-Key*` | [`HomeCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HomeCreate) | 201 [`Home`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Home) | — |
| `GET` | `/homes/{home}` | `getHome` | `bearer` / `active` | — | — | 200 [`Home`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Home) | `ETag` |
| `PATCH` | `/homes/{home}` | `updateHome` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | [`HomePatch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HomePatch) | 200 [`Home`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Home) | `ETag` |
| `GET` | `/homes/{home}/preferences` | `getHomePreferences` | `bearer` / `active` | — | — | 200 [`HomePreferences`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HomePreferences) | `ETag` |
| `PATCH` | `/homes/{home}/preferences` | `updateHomePreferences` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | [`HomePreferencesPatch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HomePreferencesPatch) | 200 [`HomePreferences`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HomePreferences) | `ETag` |

### Hubs

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `POST` | `/homes/{home}/hub-claims` | `beginHubClaim` | `bearer` / `active` | `header Idempotency-Key*` | [`HubClaimBegin`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubClaimBegin) | 201 [`HubClaimChallenge`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubClaimChallenge) | — |
| `GET` | `/homes/{home}/hubs` | `listOwnedHubs` | `bearer` / `active` | — | — | 200 object `{ "hubs": [HubOwnership] }` | — |
| `POST` | `/hubs/claim` | `finishHubClaim` | `bearer` / `active` | `header Idempotency-Key*` | [`HubClaimFinish`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubClaimFinish) | 200 [`HubOwnership`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubOwnership) | — |
| `DELETE` | `/homes/{home}/hubs/{hub}` | `unclaimHub` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | — | 204 — | — |
| `POST` | `/hubs/{hub}/enroll` | `enrollHub` | `hub-enroll` / `active` | — | [`HubEnroll`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubEnroll) | 200 [`HubCertificate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubCertificate) | — |
| `POST` | `/hubs/{hub}/certificate` | `renewHubCertificate` | `hub-mtls` / `active` | — | [`HubRenew`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubRenew) | 200 [`HubCertificate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubCertificate) | — |
| `GET` | `/hubs/{hub}/ownership` | `getHubOwnershipStatus` | `hub-mtls` / `active` | — | — | 200 [`HubOwnershipStatus`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubOwnershipStatus) | — |
| `GET` | `/hubs/{hub}/sensors` | `listHubSensorMappings` | `hub-mtls` / `active` | — | — | 200 [`HubSensorMapping`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubSensorMapping) | — |
| `GET` | `/hubs/{hub}/attachments` | `listHubAttachmentRevisions` | `hub-mtls` / `active` | — | — | 200 [`HubAttachmentFeed`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HubAttachmentFeed) | — |
| `POST` | `/hubs/{hub}/sync/batches` | `uploadHubTelemetryBatch` | `hub-mtls` / `active` | — | [`TelemetryBatch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-TelemetryBatch) | 200 [`TelemetryAcknowledgement`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-TelemetryAcknowledgement) | — |
| `POST` | `/hubs/{hub}/kasa-runtime/snapshots` | `uploadKasaRuntimeSnapshots` | `hub-mtls` / `active` | — | [`KasaRuntimeUpload`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-KasaRuntimeUpload) | 200 [`KasaRuntimeAck`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-KasaRuntimeAck) | — |
| `GET` | `/hubs/{hub}/energy-categories` | `getHubEnergyCategories` | `hub-mtls` / `active` | `header If-None-Match` | — | 200 [`energy-categories.schema.json`](../../energylens-common-contracts/api/energy-categories.schema.json) or 304 | `ETag` |
| `GET` | `/hubs/{hub}/energy-resolutions/{appliance}` | `getHubEnergyResolution` | `hub-mtls` / `active` | — | — | 200 [`datawiz-energy-resolution.schema.json`](../../energylens-common-contracts/api/datawiz-energy-resolution.schema.json) | `Cache-Control` |

### Energy categories

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/energy-categories` | `listEnergyCategories` | `bearer` / `active` | `header If-None-Match` | — | 200 grouped `{version, groups}` or 304 | `ETag` |

### Inventory

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/appliances` | `listAppliances` | `bearer` / `active` | — | — | 200 object | `Cache-Control`, `X-EnergyLens-Data-Mode`, `X-EnergyLens-Fixture-Revision`, `X-EnergyLens-Inventory-Revision` |
| `POST` | `/homes/{home}/appliances` | `createAppliance` | `bearer` / `active` | `header Idempotency-Key*` | [`ApplianceCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ApplianceCreate) | 201 [`Appliance`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Appliance) | — |
| `GET` | `/homes/{home}/appliances/{appliance}` | `getAppliance` | `bearer` / `active` | — | — | 200 [`Appliance`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Appliance) | `Cache-Control`, `ETag`, `X-EnergyLens-Data-Mode`, `X-EnergyLens-Fixture-Revision` |
| `GET` | `/homes/{home}/appliances/{appliance}/energy-resolution` | `getApplianceEnergyResolution` | `bearer` / `active` | — | — | 200 [`datawiz-energy-resolution.schema.json`](../../energylens-common-contracts/api/datawiz-energy-resolution.schema.json) | `Cache-Control` |
| `POST` | `/homes/{home}/appliances/{appliance}/energy-resolution/refresh` | `refreshApplianceEnergyResolution` | `bearer` / `active` | `header Idempotency-Key*` | — | 202 object | `Cache-Control`; 429 `Retry-After` |
| `PATCH` | `/homes/{home}/appliances/{appliance}` | `updateAppliance` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | [`AppliancePatch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AppliancePatch) | 200 [`Appliance`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Appliance) | `ETag` |
| `DELETE` | `/homes/{home}/appliances/{appliance}` | `archiveAppliance` | `bearer` / `active` | `header If-Match*`, `header Idempotency-Key*` | — | 204 — | — |
| `GET` | `/homes/{home}/sensors` | `listSensors` | `bearer` / `active` | — | — | 200 object | — |
| `POST` | `/homes/{home}/sensors` | `createSensor` | `bearer` / `active` | `header Idempotency-Key*` | [`SensorCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-SensorCreate) | 201 [`Sensor`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Sensor) | — |
| `GET` | `/homes/{home}/attachments` | `listAttachments` | `bearer` / `active` | — | — | 200 object | — |
| `POST` | `/homes/{home}/attachments` | `attachSensor` | `bearer` / `active` | `header Idempotency-Key*`, `header If-Match*` | [`AttachmentCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AttachmentCreate) | 201 [`Attachment`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Attachment) | — |
| `PATCH` | `/homes/{home}/attachments/{attachment}` | `closeAttachment` | `bearer` / `active` | `header Idempotency-Key*`, `header If-Match*` | [`AttachmentClose`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-AttachmentClose) | 200 [`Attachment`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Attachment) | — |

Owner energy-resolution refresh queues one Datawiz re-resolution (reason `owner_refresh`) and returns 202 `{appliance_id, reason, queued_at}`. Body must be empty. An appliance already in the queue returns its existing `queued_at` and records no new queue write or attempt. A repeated `Idempotency-Key` replays the stored 202 body. Limits: 1 accepted refresh per appliance per 10 minutes and 30 per home per day; over the limit returns 429 `refresh_rate_limited` with `details.retry_after_seconds` and a `Retry-After` header. 404 for unknown, archived or non-owned appliances. A missing or non-UUID `Idempotency-Key`, or a non-empty body, returns 400 `invalid_request`.

### Catalog

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `POST` | `/catalog/model-search` | `searchCatalogModels` | `bearer` / `active` | `header Idempotency-Key*` | [`ModelSearch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ModelSearch) | 200 object | `ETag`, `X-Catalog-Generation`, `X-Catalog-Response-SHA256` |
| `GET` | `/catalog/products/{product}` | `getCatalogProduct` | `bearer` / `active` | — | — | 200 [`CatalogProduct`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CatalogProduct) | `ETag`, `X-Catalog-Generation`, `X-Catalog-Response-SHA256` |
| `POST` | `/hubs/{hub}/catalog/model-search` | `searchHubCatalogModels` | `hub-mtls` / `active` | `header Idempotency-Key*` | [`ModelSearch`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ModelSearch) | 200 object | `ETag`, `X-Catalog-Generation`, `X-Catalog-Response-SHA256` |
| `GET` | `/hubs/{hub}/catalog/products/{product}` | `getHubCatalogProduct` | `hub-mtls` / `active` | — | — | 200 [`CatalogProduct`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CatalogProduct) | `ETag`, `X-Catalog-Generation`, `X-Catalog-Response-SHA256` |

### Evidence

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/usage-facts` | `findReviewedUsageFacts` | `bearer` / `active` | `category*`, `manufacturer*`, `model*`, `fuel*`, `variant*` | — | 200 [`ReviewedUsageLookup`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReviewedUsageLookup) | `Cache-Control` |
| `GET` | `/hubs/{hub}/usage-facts` | `findHubReviewedUsageFacts` | `hub-mtls` / `active` | `category*`, `manufacturer*`, `model*`, `fuel*`, `variant*` | — | 200 [`ReviewedUsageLookup`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReviewedUsageLookup) | `Cache-Control` |
| `GET` | `/homes/{home}/tariffs` | `listTariffs` | `bearer` / `active` | — | — | 200 object | — |
| `POST` | `/homes/{home}/tariffs` | `createTariffRevision` | `bearer` / `active` | `header Idempotency-Key*` | [`TariffCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-TariffCreate) | 201 [`Tariff`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-Tariff) | — |
| `GET` | `/homes/{home}/utility-rate-plans` | `listUtilityRatePlans` | `bearer` / `active` | — | — | 200 [`UtilityRatePlanList`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-UtilityRatePlanList) | — |
| `PUT` | `/homes/{home}/electric-utility` | `putElectricUtility` | `bearer` / `active` | `header Idempotency-Key*` | [`ElectricUtilityChoice`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ElectricUtilityChoice) | 200 [`UtilityRatePlanList`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-UtilityRatePlanList) | — |
| `PUT` | `/homes/{home}/utility-rate-plan` | `putUtilityRatePlan` | `bearer` / `active` | `header Idempotency-Key*` | [`UtilityRatePlanChoice`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-UtilityRatePlanChoice) | 200 [`UtilityRatePlanList`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-UtilityRatePlanList) | — |
| `GET` | `/homes/{home}/questions` | `getQuestions` | `bearer` / `active` | `purpose*`, `appliance_id` | — | 200 object | — |
| `GET` | `/homes/{home}/questions/answers` | `getCurrentQuestionAnswers` | `bearer` / `active` | `appliance_id*` | — | 200 object | — |
| `POST` | `/homes/{home}/questions/answers` | `submitQuestionAnswers` | `bearer` / `active` | `header Idempotency-Key*` | object | 201 object | — |
| `GET` | `/homes/{home}/recommendations` | `listRecommendations` | `bearer` / `active` | `capability` | — | 200 [`RecommendationPage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-RecommendationPage) | — |
| `GET` | `/homes/{home}/top-savings-move` | `getTopSavingsMove` | `bearer` / `active` | — | — | 200 [`TopSavingsMove`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-TopSavingsMove) | — |
| `POST` | `/homes/{home}/recommendation-events` | `recordRecommendationEvent` | `bearer` / `active` | `header Idempotency-Key*`, `capability` | [`RecommendationLifecycleEvent`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-RecommendationLifecycleEvent) | 200 [`RecommendationLifecycleEvent`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-RecommendationLifecycleEvent) | — |
| `GET` | `/homes/{home}/faq/answers/{question}` | `getFaqAnswer` | `bearer` / `active` | `appliance_id` | — | 200 [`FaqAnswer`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-FaqAnswer) | — |
| `POST` | `/homes/{home}/appliances/{appliance}/cycle-comparison` | `compareTaggedDishwasherCycle` | `bearer` / `active` | — | [`CycleComparisonRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CycleComparisonRequest) | 200 [`CycleComparisonResult`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CycleComparisonResult) | — |
| `GET` | `/homes/{home}/appliances/{appliance}/cycle-comparison/reference` | `getDishwasherCycleReference` | `bearer` / `active` | — | — | 200 [`CycleReferenceOption`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CycleReferenceOption) | — |
| `POST` | `/homes/{home}/appliances/{appliance}/cycle-comparison/evidence` | `saveDishwasherCycleEvidence` | `bearer` / `active` | `header Idempotency-Key*` | [`CycleComparisonRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-CycleComparisonRequest) | 201 [`SavedCycleEvidence`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-SavedCycleEvidence) | — |
| `GET` | `/homes/{home}/appliances/{appliance}/cycle-comparison/evidence/{id}` | `getDishwasherCycleEvidence` | `bearer` / `active` | — | — | 200 [`SavedCycleEvidence`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-SavedCycleEvidence) | — |

### Estimates

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/estimates` | `listEstimates` | `bearer` / `active` | `appliance_id` | — | 200 object | — |

### Rates

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/energy/rates` | `getRegionalElectricityRate` | `public` / `active` | `country*`, `postal_code*` | — | 200 [`energy_rate.schema.json`](../../energylens-common-contracts/api/v1/energy_rate.schema.json) | — |

### Mobile-Design

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/design-pages/{page}` | `getMobileDesignPage` | `bearer` / `gated` | — | — | 200 [`MobileDesignPageResponse`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-MobileDesignPageResponse) | — |
| `GET` | `/homes/{home}/dashboard` | `getHouseholdDashboard` | `bearer` / `active` | — | — | 200 [`HouseholdDashboard`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HouseholdDashboard) | — |
| `GET` | `/homes/{home}/energy-trends` | `getHouseholdEnergyTrends` | `bearer` / `active` | `granularity*`, `window` | — | 200 [`HouseholdEnergyTrends`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HouseholdEnergyTrends) | — |
| `GET` | `/homes/{home}/device-energy` | `getHouseholdDeviceEnergy` | `bearer` / `planned` | `sort`, `cursor` | — | 200 [`HouseholdDeviceEnergy`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-HouseholdDeviceEnergy) | — |

### Telemetry

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/metered-history` | `listMeteredHistory` | `bearer` / `active` | `from`, `to`, `sensor_id`, `attachment_id`, `limit`, `cursor` | — | 200 [`MeteredHistoryPage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-MeteredHistoryPage) | — |
| `GET` | `/homes/{home}/metered-summaries` | `listMeteredSummaries` | `bearer` / `active` | `granularity*`, `from`, `to`, `sensor_id`, `attachment_id`, `limit`, `cursor` | — | 200 [`MeteredSummaryPage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-MeteredSummaryPage) | `Cache-Control`, `X-EnergyLens-Data-Mode`, `X-EnergyLens-Evidence-Revision`, `X-EnergyLens-Fixture-Revision` |
| `GET` | `/homes/{home}/kasa-runtime` | `listKasaRuntimeSnapshots` | `bearer` / `active` | `limit`, `cursor` | — | 200 [`KasaRuntimePage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-KasaRuntimePage) | — |

### Reports

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `POST` | `/homes/{home}/reports/preview` | `createReportPreview` | `bearer` / `active` | `header Idempotency-Key*` | object | 201 [`ReportPreview`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReportPreview) | — |
| `GET` | `/homes/{home}/reports/previews/{preview}` | `getReportPreview` | `bearer` / `active` | — | — | 200 [`ReportPreview`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReportPreview) | — |
| `GET` | `/homes/{home}/reports/previews/{preview}/pdf` | `getReportPreviewPdf` | `bearer` / `active` | `header X-Report-Snapshot-SHA256*` | — | 200 `application/pdf` | `Cache-Control`, `Content-Disposition` |
| `POST` | `/homes/{home}/reports/previews/{preview}/deliveries` | `queueManualReportDelivery` | `bearer` / `active` | `header Idempotency-Key*` | [`ReportDeliveryCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReportDeliveryCreate) | 201 [`ReportDeliveryRequest`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ReportDeliveryRequest) | — |

### Metering

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/metered-export` | `exportMeteredRecords` | `bearer` / `active` | `dataset`, `from`, `to`, `sensor_id`, `attachment_id`, `limit`, `cursor` | — | 200 [`MeteredExportPage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-MeteredExportPage) | — |

### Programs

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `GET` | `/homes/{home}/programs` | `listReviewedProgramsForAppliance` | `bearer` / `active` | `appliance_id*` | — | 200 [`OwnerProgramPage`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-OwnerProgramPage) | — |
| `POST` | `/homes/{home}/programs/discovery-refresh` | `requestProgramDiscoveryRefresh` | `bearer` / `active` | `header Idempotency-Key*` | [`ProgramDiscoveryRefreshCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ProgramDiscoveryRefreshCreate) | 202 [`ProgramDiscoveryRefreshStatus`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ProgramDiscoveryRefreshStatus) | — |
| `GET` | `/homes/{home}/programs/discovery-refresh/{request}` | `getProgramDiscoveryRefresh` | `bearer` / `active` | — | — | 200 [`ProgramDiscoveryRefreshStatus`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-ProgramDiscoveryRefreshStatus) | — |

### Recommendations

| Method | Path | Operation | Auth / status | Parameters | Request | Success response | Response headers |
|---|---|---|---|---|---|---|---|
| `POST` | `/homes/{home}/interventions` | `recordManualIntervention` | `bearer` / `active` | `header Idempotency-Key*` | [`InterventionCreate`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-InterventionCreate) | 201 [`InterventionCreated`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-InterventionCreated) | — |
| `GET` | `/homes/{home}/interventions/{id}/outcome` | `getManualInterventionOutcome` | `bearer` / `active` | — | — | 200 [`InterventionOutcome`](../../energylens-common-contracts/api/cloud-openapi.yaml#components-schemas-InterventionOutcome) | — |


## Headers and errors

- JSON bodies use `Content-Type: application/json` where declared. Operation-specific required headers and query parameters appear in the OpenAPI contract.
- `If-Match` carries the current ETag for conditional updates; `Idempotency-Key` supports safe replay for specified mutations.
- Response validators, cache controls, and revision metadata (including `ETag`, `Cache-Control`, catalog generation/hash, and fixture/inventory revisions) are declared per operation.
- OpenAPI documents operation error statuses and the shared error shape. Handlers also return plain-text errors on some bootstrap paths. `/healthz` returns `ok`; `/readyz` returns `ready` or `not ready`.

## Seeded and persisted home data

- `GET /v1/homes/{home}/appliances` combines seeded demo appliances with persisted active appliances when both exist. Its `X-EnergyLens-Data-Mode` is `mixed`; `X-EnergyLens-Fixture-Revision` identifies the seeded snapshot. If the list contains only seeded rows, mode is `sample`. `X-EnergyLens-Inventory-Revision` versions the combined inventory state.
- `GET /v1/homes/{home}/appliances/{appliance}` reports provenance per row. A seeded detail response has `X-EnergyLens-Data-Mode: sample` and its fixture revision even when the list overall is mixed. Seeded row details use the demo Appliance variant and do not become editable persisted appliances.
- Dashboard and trend responses use top-level `data_mode: mixed` and `fixture_revision` when seeded allocations accompany persisted inventory or measured intervals. Seeded device allocations remain alongside live totals/bucket allocations; they are separate allocations and must not be added to those totals.
- If persisted appliances exist but there are no measured intervals, seeded snapshot totals remain the available totals. Once intervals exist, the aggregate is based on measured intervals while seeded per-device allocations remain separately available.
- In mixed energy responses seeded rows identify `device_id` and `name`; persisted rows identify `appliance_id`. Allocation fields retain their existing schema.
- Electricity revisions rank a user tariff or a user-picked rate plan first, then other official revisions (the default plan, operator imports); within a rank the latest revision wins. A revision's price at an instant is its `rate` plus the adder of the `tariff_revision_periods` row covering the home-local month, weekday and wall-clock time (0 when none); a wrapping row's early hours belong to its listed day. The worker applies the utility's standard plan as the default and refreshes plan revisions daily, and every 5 minutes for homes whose location changed.
- A single root utility meter is the measured whole-home source. Its complete topology may support device allocations from attached terminal plug or outlet descendants; child and intermediate meter energy is never added to the home total. Without a root utility meter, distinct terminal plug or outlet loads form the total, including outlets below circuit or strip meters, while their metering ancestors and unattached branches are excluded. Overlapping duplicate or shared-sensor measurements are excluded from affected device rows while independent branches remain available. A home whose tariff is missing or older than 90 days (by `source_as_of`) has whole-home totals and measured device cost priced at the US average residential rate with `rate_basis: us_average`; cost is null only when no rate is available. Measured device cost is `unclassified_cost` until the datawiz record splits it: waste comes from the standby spec, else the record's `waste_floor` converted at the home rate (the US average residential rate with `rate_basis: us_average` when the home has no tariff); a device with no record stays unsplit. Estimated rows from datawiz records carry the same `waste_basis` and `rate_basis`, and `listEstimates` returns `no_energy_data` for an `unresolved` record.

## Mount and contract drift

- **Documented but not mounted:** `GET /v1/homes/{home}/device-energy` is marked `planned` and has no Go route registration. It is not callable.
- **Conditionally mounted:** `GET /v1/homes/{home}/design-pages/{page}` requires the sample-fixtures feature flag and allowlisted user and home IDs. Other callers receive not found.
- **Local auth variant:** local auth mounts the documented `/v1/auth/local/*` operations when enabled. The username/password profile accepts direct User ID reset at `/reset-password` with `username` and a new `password`; validly shaped known, unknown, and throttled requests all receive 204, while an actual reset revokes existing sessions. This profile does not mount email challenge recovery. Development auth reuses `/login`, `/refresh`, and `/logout` at those paths when dev auth is enabled, with in-memory sessions. Dev auth does not implement registration, verification, recovery, or reset-password.
- **Email signup:** `GET /v1/auth/config` advertises email registration without required verification. New email signup returns a session immediately. A duplicate or throttled email request returns generic 202 without tokens. `/v1/me` reports `email_verified: false` until mailbox proof through a recovery challenge. Owned home list/read responses always include nullable `first_device_added_at`; only a persisted real appliance sets it, and archive does not clear it.
- Provisioning/import/migration commands and worker jobs are not HTTP operations.
- OpenAPI describes the mixed seeded/persisted behavior above. `api/check_cloud_contract.py` validates the schema, route manifest, and golden payloads.

## Sources

- Router/module mounts: [`internal/app/app.go`](../internal/app/app.go).
- Handlers: `energylens-cloud/internal/<module>/*http*.go` and service files, grouped above by OpenAPI tag.
- Canonical contract: [`cloud-openapi.yaml`](../../energylens-common-contracts/api/cloud-openapi.yaml).
- Generated route/auth index: [`cloud-route-manifest.json`](../../energylens-common-contracts/api/cloud-route-manifest.json), generated by [`cloud_route_manifest.py`](../../energylens-common-contracts/api/cloud_route_manifest.py).
