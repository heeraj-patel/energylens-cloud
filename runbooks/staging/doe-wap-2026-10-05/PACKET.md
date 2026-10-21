# DOE WAP staging packet — October 5, 2026

This packet tests one general household entry for the U.S. Department of Energy's Weatherization Assistance Program (WAP). It does not register a production source, publish an offer, confirm local enrollment, or estimate an award. The entry's availability status is `open` **only for DOE's national application and inquiry route**: [DOE's current application page](https://www.energy.gov/cmei/scep/wap/how-apply-weatherization-assistance) tells households how to contact a state administrator and local provider. It does not establish open intake or available funds at a particular provider. Income, provider capacity, waitlist, dwelling permission, and the audit still control service.

## Capture and access review

On October 5, 2026, [DOE robots.txt](https://www.energy.gov/robots.txt) had no disallow rule for the WAP application path. [DOE web policies](https://www.energy.gov/web-policies) state that DOE government information is public domain, request DOE credit, and ask others to link to the live site instead of reposting downloaded portions. One HTTPS GET returned status 200 with no redirect and `text/html; charset=utf-8`. The exact 135,665 captured bytes hash to `05d32356150408fec895bc2751a369e20273abafa471e3e1fd959cab9bf4fc74`. They are stored privately at the path in `capture.json`; the official HTML is not in Git. The customer source link remains DOE's live page. The registered source is manual and has no discovery mapping or scheduled fetch.

`review.json` cites literal text fragments in those bytes for each required field. The builder rejects missing fragments, a changed hash, changed byte count, or mismatched source metadata. Evidence for `status=open` points to DOE's published instructions to start the state and local application process; it does not claim every local provider is currently accepting households. Empty postal codes with country scope represent a national route for inquiry, not confirmed local service. `requires_income`, `requires_address`, and `additional_conditions` remain true, so the owner feed still assesses household status as `unknown`. No application, purchase, installation, or reservation dates are asserted; the review expires October 12, 2026.

## Owner-approved exact customer words

The owner approved the following exact words on October 5, 2026. They are recorded in `proposed-copy.json` and are used only by the local staging builder. Approval of these words does not approve program eligibility, local availability, or a production import:

| Field and location | Proposed text |
| --- | --- |
| Programs row title | “Weatherization Assistance Program” |
| Programs row short benefit | “Weatherization services” |
| Programs label | “Federal” |
| Offer source link text | “U.S. Department of Energy” |

The source link points to DOE's official application page. The staging builder joins the approved title and benefit to the review only for a temporary local database test. A refreshed source and status review will be needed before any live import because this packet expires quickly.

## Reproduce the local integration test

From `energylens-cloud`, use the retained private capture. The builder writes a generated artifact outside Git:

```sh
python3 runbooks/staging/doe-wap-2026-10-05/build_staging_artifact.py \
  --staging \
  --capture /Users/nik/.local/share/energylens/program-captures/doe-wap-2026-10-05/source.html \
  --output /tmp/doe-wap-staging-review.json

createdb energylens_program_staging_wap_check
ENERGYLENS_CLOUD_TEST_DATABASE_URL='postgres://localhost/energylens_program_staging_wap_check?sslmode=disable' \
ENERGYLENS_WAP_CAPTURE_PATH='/Users/nik/.local/share/energylens/program-captures/doe-wap-2026-10-05/source.html' \
ENERGYLENS_WAP_STAGING_ARTIFACT='/tmp/doe-wap-staging-review.json' \
go test ./cmd/energylens-cloud -run TestDOEWAPStagingOwnerFeed -count=1
dropdb energylens_program_staging_wap_check
```

The test migrates an isolated schema, executes `register-local.sql` only in a database whose name starts `energylens_program_staging_`, imports the hash-matched capture through the operator command, creates an owned home with a U.S. ZIP, and checks that the home feed returns exactly one source-linked program with an open national inquiry route and unknown income, address, and local conditions. It removes the schema afterward. A fresh download can have different bytes and must receive a new hash, evidence check, access review, and expiry; do not overwrite this immutable capture or reuse the old packet with new bytes.

## Remaining live blockers

- The owner approved the exact title, short benefit, Federal label, and DOE source link text. The complete mobile presentation still needs acceptance.
- State and local current application status, provider assignment, capacity, and specific household requirements remain unverified. A national application route is not a household award.
- The staging source registration is intentionally limited to a local database. A live registration needs a fresh controlling capture, current source and access review, an operator record, and a separate live import decision.
- Mobile customer acceptance and a Release build on the physical iPhone remain outside this Cloud packet.
