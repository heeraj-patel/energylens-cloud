-- Signed KB publications are imported only by the operator command. Source
-- retrieval time is nullable because the reviewed artifact does not assert it.
CREATE TABLE catalog_publications (
  payload_sha256 text PRIMARY KEY CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
  key_id text NOT NULL CHECK (key_id ~ '^[0-9a-f]{64}$'),
  envelope_sha256 text NOT NULL CHECK (envelope_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  imported_by text NOT NULL CHECK (length(trim(imported_by)) > 0),
  product_count integer NOT NULL CHECK (product_count > 0),
  fact_count integer NOT NULL CHECK (fact_count > 0)
);
CREATE TABLE catalog_products (
  id uuid PRIMARY KEY,
  category text NOT NULL CHECK (length(trim(category)) > 0),
  brand text NOT NULL CHECK (length(trim(brand)) > 0),
  full_model text NOT NULL CHECK (length(trim(full_model)) > 0),
  variant text,
  region text,
  fuel text NOT NULL CHECK (fuel = 'electricity'),
  match_quality text NOT NULL CHECK (match_quality IN ('confirmed_exact','documented_variant_family')),
  product_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE catalog_fact_revisions (
  id uuid PRIMARY KEY,
  product_id uuid NOT NULL REFERENCES catalog_products(id),
  publication_sha256 text NOT NULL REFERENCES catalog_publications(payload_sha256),
  kind text NOT NULL CHECK (kind = 'annual_energy'),
  value numeric NOT NULL CHECK (value > 0),
  unit text NOT NULL CHECK (unit = 'kWh'),
  test_basis jsonb NOT NULL,
  included_energy jsonb NOT NULL,
  source_kind text NOT NULL CHECK (source_kind IN ('epa_exact','oem_exact','oem_family','other_official')),
  source_url text NOT NULL CHECK (length(trim(source_url)) > 0),
  source_hash text NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
  source_hash_basis text NOT NULL CHECK (source_hash_basis IN ('verified_source_document','reviewed_row_manifest')),
  source_manifest text NOT NULL CHECK (length(trim(source_manifest)) > 0),
  source_fetched_at timestamptz,
  reviewed_at timestamptz NOT NULL,
  revision integer NOT NULL CHECK (revision > 0),
  fact_json jsonb NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(product_id,kind,source_hash,revision)
);
CREATE INDEX catalog_fact_product_idx ON catalog_fact_revisions(product_id,kind,reviewed_at DESC);
CREATE FUNCTION reject_catalog_import_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'imported catalog evidence is immutable'; END $$;
CREATE TRIGGER catalog_publications_immutable BEFORE UPDATE OR DELETE ON catalog_publications FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER catalog_products_immutable BEFORE UPDATE OR DELETE ON catalog_products FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER catalog_facts_immutable BEFORE UPDATE OR DELETE ON catalog_fact_revisions FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
