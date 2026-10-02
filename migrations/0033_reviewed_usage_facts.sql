-- Operator-imported explicit per-cycle and per-hour facts are separate from
-- annual catalog ratings. Neither table is exposed by a customer route.
CREATE TABLE reviewed_usage_publications (
  payload_sha256 text PRIMARY KEY CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
  key_id text NOT NULL CHECK (key_id ~ '^[0-9a-f]{64}$'),
  envelope_sha256 text NOT NULL CHECK (envelope_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  imported_by text NOT NULL CHECK (length(trim(imported_by)) > 0),
  fact_count integer NOT NULL CHECK (fact_count > 0)
);
CREATE TABLE reviewed_usage_facts (
  id uuid PRIMARY KEY,
  publication_sha256 text NOT NULL REFERENCES reviewed_usage_publications(payload_sha256),
  category text NOT NULL,
  manufacturer text NOT NULL,
  model text NOT NULL,
  fuel text NOT NULL CHECK (fuel = 'electricity'),
  variant text NOT NULL,
  match_quality text NOT NULL CHECK (match_quality IN ('confirmed_exact','documented_variant_family')),
  documented_scope text NOT NULL,
  matched_model text NOT NULL,
  measurement_kind text NOT NULL CHECK (measurement_kind IN ('cycle_energy','hourly_energy')),
  value numeric NOT NULL CHECK (value > 0),
  unit text NOT NULL CHECK ((measurement_kind='cycle_energy' AND unit='kWh/cycle') OR (measurement_kind='hourly_energy' AND unit='kWh/hour')),
  test_basis text NOT NULL,
  cycle_definition text,
  operation_mode text,
  included_energy jsonb NOT NULL CHECK (included_energy @> '{"status":"documented","components":["appliance_electricity"]}'::jsonb),
  source_kind text NOT NULL CHECK (source_kind IN ('oem_pdf','epa_row')),
  source_url text NOT NULL,
  source_sha256 text NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  source_page_index integer,
  dataset_id text,
  row_identifier text,
  field_name text,
  metadata_sha256 text,
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  reviewer text NOT NULL,
  reviewed_at timestamptz NOT NULL,
  source_manifest text NOT NULL,
  fact_json jsonb NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((measurement_kind='cycle_energy' AND cycle_definition IS NOT NULL AND operation_mode IS NULL)
      OR (measurement_kind='hourly_energy' AND operation_mode IS NOT NULL AND cycle_definition IS NULL)),
  CHECK ((source_kind='oem_pdf' AND source_page_index IS NOT NULL AND source_page_index >= 0
          AND dataset_id IS NULL AND row_identifier IS NULL AND field_name IS NULL AND metadata_sha256 IS NULL)
      OR (source_kind='epa_row' AND source_page_index IS NULL AND dataset_id IS NOT NULL
          AND row_identifier IS NOT NULL AND field_name IS NOT NULL AND metadata_sha256 ~ '^[0-9a-f]{64}$'))
);
CREATE INDEX reviewed_usage_facts_identity_idx ON reviewed_usage_facts(lower(manufacturer),lower(model),fuel,variant,measurement_kind);
-- Until an explicit supersession protocol exists, importing a second fact for
-- one model/variant/kind fails. Useful-cost selection cannot become ambiguous.
CREATE UNIQUE INDEX reviewed_usage_facts_one_identity_idx ON reviewed_usage_facts(lower(category),lower(manufacturer),regexp_replace(upper(model), '[^A-Z0-9]', '', 'g'),fuel,lower(variant),measurement_kind);
CREATE TRIGGER reviewed_usage_publications_immutable BEFORE UPDATE OR DELETE ON reviewed_usage_publications FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER reviewed_usage_facts_immutable BEFORE UPDATE OR DELETE ON reviewed_usage_facts FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
