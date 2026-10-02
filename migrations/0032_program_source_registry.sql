-- Public program discovery and reviewed terms. These records are independent
-- of accounts and homes; a ZIP/provider candidate never proves household service.
CREATE TABLE program_sources (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  source_key text NOT NULL UNIQUE CHECK (length(trim(source_key)) BETWEEN 1 AND 160),
  source_kind text NOT NULL CHECK (source_kind IN ('government','administrator','utility','manufacturer','directory')),
  authority_name text NOT NULL CHECK (length(trim(authority_name)) > 0),
  administrator_name text,
  canonical_url text NOT NULL CHECK (canonical_url ~ '^https://'),
  allowed_hosts text[] NOT NULL CHECK (cardinality(allowed_hosts) > 0),
  access_method text NOT NULL CHECK (access_method IN ('api','feed','html','pdf','manual')),
  terms_url text NOT NULL CHECK (terms_url ~ '^https://'),
  terms_reviewed_at timestamptz NOT NULL,
  terms_review_expires_at timestamptz NOT NULL,
  terms_reviewed_by text NOT NULL CHECK (length(trim(terms_reviewed_by)) > 0),
  parser_version text NOT NULL CHECK (length(trim(parser_version)) > 0),
  canonicalization_version text NOT NULL CHECK (length(trim(canonicalization_version)) > 0),
  geography_coverage jsonb NOT NULL CHECK (jsonb_typeof(geography_coverage) = 'object'),
  category_coverage jsonb NOT NULL CHECK (jsonb_typeof(category_coverage) = 'array'),
  refresh_interval interval NOT NULL CHECK (refresh_interval > interval '0 seconds'),
  max_requests_per_run integer NOT NULL CHECK (max_requests_per_run > 0),
  max_response_bytes bigint NOT NULL CHECK (max_response_bytes > 0),
  last_success_at timestamptz,
  last_failure_at timestamptz,
  next_due_at timestamptz,
  enabled boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (terms_review_expires_at > terms_reviewed_at)
);

CREATE TABLE program_source_snapshots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  source_id uuid NOT NULL REFERENCES program_sources(id),
  source_url text NOT NULL CHECK (source_url ~ '^https://'),
  content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
  content_type text NOT NULL CHECK (length(trim(content_type)) > 0),
  content_bytes bigint NOT NULL CHECK (content_bytes > 0),
  fetched_at timestamptz NOT NULL,
  parser_version text NOT NULL CHECK (length(trim(parser_version)) > 0),
  snapshot_ref text NOT NULL CHECK (length(trim(snapshot_ref)) > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source_id, source_url, content_sha256)
);
CREATE INDEX program_source_snapshots_recent_idx ON program_source_snapshots(source_id, fetched_at DESC);

CREATE TABLE program_discovery_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  country_code char(2) NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
  postal_code text NOT NULL CHECK (length(trim(postal_code)) > 0),
  state_code text,
  county_name text,
  city_name text,
  category text,
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  state text NOT NULL CHECK (state IN ('running','complete','partial','failed')),
  limitations jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(limitations) = 'array'),
  CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX program_discovery_runs_location_idx ON program_discovery_runs(country_code, postal_code, started_at DESC);

-- One row per identified scope, including failed/pending checks. For utilities,
-- provider_role distinguishes distributor, retailer and fuel service roles.
CREATE TABLE program_discovery_coverage (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES program_discovery_runs(id),
  scope_kind text NOT NULL CHECK (scope_kind IN ('utility','city','county','state','federal','tribal','manufacturer')),
  scope_key text NOT NULL CHECK (length(trim(scope_key)) > 0),
  scope_name text NOT NULL CHECK (length(trim(scope_name)) > 0),
  provider_role text CHECK (provider_role IN ('electric_distributor','electric_retailer','gas_distributor','gas_retailer','other_fuel')),
  source_id uuid REFERENCES program_sources(id),
  status text NOT NULL CHECK (status IN ('identified','checked','failed','pending')),
  checked_at timestamptz,
  failure_code text,
  retry_after timestamptz,
  notes text,
  UNIQUE NULLS NOT DISTINCT (run_id, scope_kind, scope_key, provider_role),
  CHECK ((scope_kind = 'utility') = (provider_role IS NOT NULL)),
  CHECK (status <> 'checked' OR checked_at IS NOT NULL),
  CHECK (status <> 'failed' OR length(trim(failure_code)) > 0)
);
CREATE INDEX program_discovery_coverage_run_idx ON program_discovery_coverage(run_id, scope_kind, status);

CREATE TABLE public_programs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  administrator_key text NOT NULL CHECK (length(trim(administrator_key)) > 0),
  program_key text NOT NULL CHECK (length(trim(program_key)) > 0),
  geography_key text NOT NULL CHECK (length(trim(geography_key)) > 0),
  administrator_name text NOT NULL CHECK (length(trim(administrator_name)) > 0),
  source_id uuid NOT NULL REFERENCES program_sources(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (administrator_key, program_key, geography_key)
);

-- Extracted candidate terms cannot be served until independently reviewed.
-- Separate windows avoid assuming that purchase, installation, reservation and
-- application dates are interchangeable. JSON terms retain unknown values.
CREATE TABLE public_program_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  program_id uuid NOT NULL REFERENCES public_programs(id),
  revision integer NOT NULL CHECK (revision > 0),
  source_snapshot_id uuid NOT NULL REFERENCES program_source_snapshots(id),
  lifecycle text NOT NULL CHECK (lifecycle IN ('candidate','reviewed','rejected')),
  status text NOT NULL CHECK (status IN ('proposed','upcoming','open','waitlist','exhausted','suspended','closed','unknown')),
  title text NOT NULL CHECK (length(trim(title)) > 0),
  benefit_kind text NOT NULL CHECK (benefit_kind IN ('rebate','tax_credit','bill_credit','loan','demand_response_reward','other')),
  benefit_terms jsonb NOT NULL CHECK (jsonb_typeof(benefit_terms) = 'object'),
  eligibility_terms jsonb NOT NULL CHECK (jsonb_typeof(eligibility_terms) = 'object'),
  stacking_terms jsonb NOT NULL CHECK (jsonb_typeof(stacking_terms) = 'object'),
  purchase_from date,
  purchase_to date,
  installation_from date,
  installation_to date,
  reservation_from date,
  reservation_to date,
  application_from date,
  application_to date,
  extracted_at timestamptz NOT NULL,
  reviewed_at timestamptz,
  review_expires_at timestamptz,
  reviewed_by text,
  review_note text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (program_id, revision),
  CHECK (purchase_to IS NULL OR purchase_from IS NULL OR purchase_to >= purchase_from),
  CHECK (installation_to IS NULL OR installation_from IS NULL OR installation_to >= installation_from),
  CHECK (reservation_to IS NULL OR reservation_from IS NULL OR reservation_to >= reservation_from),
  CHECK (application_to IS NULL OR application_from IS NULL OR application_to >= application_from),
  CHECK ((lifecycle = 'candidate' AND reviewed_at IS NULL AND review_expires_at IS NULL AND reviewed_by IS NULL)
      OR (lifecycle IN ('reviewed','rejected') AND reviewed_at IS NOT NULL
          AND review_expires_at > reviewed_at AND length(trim(reviewed_by)) > 0))
);
CREATE INDEX public_program_revisions_latest_idx ON public_program_revisions(program_id, revision DESC);

CREATE TABLE public_program_evidence (
  revision_id uuid NOT NULL REFERENCES public_program_revisions(id),
  field_path text NOT NULL CHECK (length(trim(field_path)) > 0),
  snapshot_id uuid NOT NULL REFERENCES program_source_snapshots(id),
  locator text NOT NULL CHECK (length(trim(locator)) > 0),
  evidence_note text,
  PRIMARY KEY (revision_id, field_path, snapshot_id, locator)
);

CREATE TABLE public_program_conflicts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  revision_id uuid NOT NULL REFERENCES public_program_revisions(id),
  field_path text NOT NULL CHECK (length(trim(field_path)) > 0),
  first_snapshot_id uuid NOT NULL REFERENCES program_source_snapshots(id),
  second_snapshot_id uuid NOT NULL REFERENCES program_source_snapshots(id),
  detected_at timestamptz NOT NULL DEFAULT now(),
  details text NOT NULL CHECK (length(trim(details)) > 0),
  CHECK (first_snapshot_id <> second_snapshot_id)
);
CREATE TABLE public_program_conflict_resolutions (
  conflict_id uuid PRIMARY KEY REFERENCES public_program_conflicts(id),
  controlling_snapshot_id uuid NOT NULL REFERENCES program_source_snapshots(id),
  resolved_at timestamptz NOT NULL,
  resolved_by text NOT NULL CHECK (length(trim(resolved_by)) > 0),
  rationale text NOT NULL CHECK (length(trim(rationale)) > 0)
);
CREATE FUNCTION validate_program_conflict_resolution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM public_program_conflicts c
    WHERE c.id = NEW.conflict_id
      AND NEW.controlling_snapshot_id IN (c.first_snapshot_id, c.second_snapshot_id)
  ) THEN
    RAISE EXCEPTION 'controlling snapshot must be one of the conflicting sources';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER public_program_conflict_resolution_valid BEFORE INSERT ON public_program_conflict_resolutions
  FOR EACH ROW EXECUTE FUNCTION validate_program_conflict_resolution();

-- A review is a separate human decision, not a consequence of a fresh fetch.
-- Dynamic freshness and unresolved conflicts are checked on every read. The
-- view returns only the latest revision, so a newer candidate suppresses old
-- reviewed terms until a reviewer accepts current evidence.
CREATE VIEW current_verified_program_revisions AS
SELECT r.* FROM public_program_revisions r
JOIN public_programs p ON p.id = r.program_id
JOIN program_sources s ON s.id = p.source_id
JOIN program_source_snapshots snap ON snap.id = r.source_snapshot_id AND snap.source_id = s.id
WHERE r.revision = (SELECT max(newer.revision) FROM public_program_revisions newer WHERE newer.program_id = r.program_id)
  AND r.lifecycle = 'reviewed'
  AND r.reviewed_at <= now()
  AND r.review_expires_at > now()
  AND s.terms_reviewed_at <= now()
  AND s.terms_review_expires_at > now()
  AND snap.fetched_at <= r.reviewed_at
  AND EXISTS (SELECT 1 FROM public_program_evidence e WHERE e.revision_id = r.id)
  AND NOT EXISTS (
    SELECT 1 FROM public_program_conflicts c
    LEFT JOIN public_program_conflict_resolutions cr ON cr.conflict_id = c.id
    WHERE c.revision_id = r.id AND cr.conflict_id IS NULL
  );

CREATE FUNCTION reject_public_program_evidence_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'public program evidence is immutable'; END $$;
CREATE TRIGGER program_source_snapshot_immutable BEFORE UPDATE OR DELETE ON program_source_snapshots FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
CREATE TRIGGER public_program_revision_immutable BEFORE UPDATE OR DELETE ON public_program_revisions FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
CREATE TRIGGER public_program_evidence_immutable BEFORE UPDATE OR DELETE ON public_program_evidence FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
CREATE TRIGGER public_program_conflict_immutable BEFORE UPDATE OR DELETE ON public_program_conflicts FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
CREATE TRIGGER public_program_conflict_resolution_immutable BEFORE UPDATE OR DELETE ON public_program_conflict_resolutions FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
