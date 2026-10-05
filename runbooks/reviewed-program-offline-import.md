# Offline reviewed program import

This path records a human-reviewed public offer from an immutable official source capture. Source checks establish accuracy and current terms; they are not EnergyLens endorsement. No command here fetches a website, registers a source, or publishes a lead automatically.

## Before import

1. Recheck the official administrator's current status, available funding, application route, eligibility, benefit type, and every controlling form or PDF. Capture the exact page or PDF bytes manually, recording the final URL, retrieval time, redirects, content type, and SHA-256. Review each host's access terms and robots rules before automated fetching. The October 5 research packet is a lead list, not a current source capture.
2. Register the official authority in `program_sources` using an operator database credential only after that review. Use `access_method='manual'` for manually captured sources, the final official HTTPS host in `allowed_hosts`, reviewed access terms URL and dates, and `geography_coverage='{}'`, `category_coverage='[]'` until discovery mappings are separately verified. `enabled=true` is required for import; it is not evidence that the program is open. Do not use a directory or unreviewed lead as the controlling source. Keep the source registration record and access review in the operator change ticket.
3. Prepare one JSON artifact per *distinct offer*. Use `source_key`, `source_url`, `source_sha256`, `content_type`, `snapshot_ref`, `parser_version`, `administrator_key`, `program_key`, `geography_key`, `administrator_name`, `administrator_kind`, `status`, `title`, `benefit_kind`, `benefit_display`, `benefit_terms`, `eligibility_terms`, `stacking_terms`, `dates`, `evidence`, `reviewer`, `review_note`, `review_expires_at`, and `applicability`. The `evidence` object must cite locators in the captured bytes for all reviewed fields and every applicability field. Countrywide scope also needs `applicability.geography_scope` evidence. The `title` and `benefit_display` are customer-facing and require owner approval before a live import. Never put source bytes or a snapshot ID in JSON.
4. For a household-wide national offer, use `applicability: {"country_code":"US","geography_scope":"country","postal_codes":[],"category":"home","fuel":"any",...}` only when the controlling terms support national availability. For state, city, or utility offers, use verified five-digit `postal_codes`; do not infer address-level utility service from ZIP. Set `provider_key` and `provider_role` when a utility account is required. Set `requires_income`, `requires_address`, and `additional_conditions` conservatively. Unknown contractor, model, provider capacity, local funding, or transaction dates belong in the structured terms and must remain unknown to the household feed.

Run the import with the provisioning database role and the same TLS policy as other operator commands:

```sh
ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL='postgres://...' ENERGYLENS_CLOUD_ENV=prod \
  energylens-cloud import-reviewed-program \
  --artifact /secure/reviews/offer.json \
  --source-snapshot /secure/captures/official-page.html
```

The command checks the exact bytes against `source_sha256`, then `ImportReviewed` checks registration, host, parser version, review expiry, evidence completeness, and source size. It stores an immutable snapshot and reviewed revision atomically. The returned revision ID can be checked through the owner home Programs API for a test account after import. A later source change requires a new snapshot and review; source or review expiry removes the offer from customer reads.

## Specific review leads

| Lead | Review before one offer is importable |
| --- | --- |
| DOE WAP | Capture DOE's current application page plus controlling state/local terms. One national household entry can explain the federal route, with income and local provider capacity unknown; do not duplicate state delivery as separate additive awards. If current enrollment varies by state, use narrower reviewed geography or unknown status. |
| Texas CEAP / California LIHEAP | Capture each state administrator's current plan, application page, income criteria, subrecipient route, and availability. Treat utility-bill help as conditional assistance, not an appliance rebate. Statewide administrative coverage does not prove service or funds at a home's address. |
| Austin Energy heat-pump water heater | Capture current offer page, equipment requirements, permit and application deadline, funding language, and service requirement. Import only for reviewed ZIP candidates and preserve `provider_role='electric_distributor'` if that is the documented service role. The homeowner's Austin ZIP does not confirm an Austin Energy account. |
| LADWP Consumer Rebate Program / product marketplace | Capture the current controlling page, application, product tables, deadlines, and any official redirect host. Separate equipment offers and marketplace promotions; preserve meter requirement, model tiers, first-come funding, and purchase/application windows. Do not publish an “up to” amount as a guaranteed benefit. |

## Remaining blockers for live offers

No official capture, site access determination, source registration, approved customer display text, or live reviewed revision is included in this change. The test uses a shortened DOE-text fixture to exercise the operator command; it does not establish current funding, full terms, or a publishable WAP offer. Current ZIP mapping cannot establish utility service or local-provider capacity. Statewide programs need reviewed ZIP coverage or an explicitly justified national scope; the latter must not overstate availability. The source queue stays disabled, and no production database was changed.
