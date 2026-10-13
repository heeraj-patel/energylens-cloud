CREATE TABLE reviewed_rated_input_publications (
  payload_sha256 text PRIMARY KEY CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
  key_id text NOT NULL CHECK (key_id ~ '^[0-9a-f]{64}$'),
  envelope_sha256 text NOT NULL CHECK (envelope_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  imported_by text NOT NULL CHECK (length(trim(imported_by)) > 0),
  fact_count integer NOT NULL CHECK (fact_count >= 0)
);
CREATE TABLE reviewed_rated_input_facts (
  id uuid PRIMARY KEY,
  publication_sha256 text NOT NULL REFERENCES reviewed_rated_input_publications(payload_sha256),
  category text NOT NULL,
  manufacturer text NOT NULL,
  model text NOT NULL,
  source_kind text NOT NULL CHECK (source_kind = 'oem_rated_input'),
  matched_model text NOT NULL,
  rated_input_watts numeric NOT NULL CHECK (rated_input_watts BETWEEN 0.1 AND 50000),
  unit text NOT NULL CHECK (unit = 'W'),
  source_url text NOT NULL,
  manufacturer_source_url text NOT NULL,
  source_sha256 text NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  usage_basis text NOT NULL,
  source_manifest text NOT NULL,
  fact_json jsonb NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reviewed_rated_input_identity_idx
  ON reviewed_rated_input_facts(lower(category),
    regexp_replace(upper(manufacturer),'[^A-Z0-9]','','g'),
    regexp_replace(upper(model),'[^A-Z0-9]','','g'));
CREATE TABLE reviewed_rated_input_revisions (
  id uuid PRIMARY KEY,
  publication_sha256 text NOT NULL REFERENCES reviewed_rated_input_publications(payload_sha256),
  prior_fact_id uuid NOT NULL UNIQUE REFERENCES reviewed_rated_input_facts(id),
  replacement_fact_id uuid UNIQUE REFERENCES reviewed_rated_input_facts(id),
  action text NOT NULL CHECK (action IN ('supersede','retract')),
  reason text NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 1000),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((action='supersede' AND replacement_fact_id IS NOT NULL AND replacement_fact_id<>prior_fact_id)
      OR (action='retract' AND replacement_fact_id IS NULL))
);
CREATE TRIGGER reviewed_rated_input_publications_immutable BEFORE UPDATE OR DELETE ON reviewed_rated_input_publications
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER reviewed_rated_input_facts_immutable BEFORE UPDATE OR DELETE ON reviewed_rated_input_facts
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER reviewed_rated_input_revisions_immutable BEFORE UPDATE OR DELETE ON reviewed_rated_input_revisions
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE VIEW active_reviewed_rated_input_facts AS
SELECT f.* FROM reviewed_rated_input_facts f
WHERE NOT EXISTS (SELECT 1 FROM reviewed_rated_input_revisions r WHERE r.prior_fact_id=f.id);
