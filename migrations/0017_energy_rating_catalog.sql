-- 0017_energy_rating_catalog.sql — durable KB rating evidence for later,
-- explicitly reviewed distribution. This table is not read by the rate API.
-- Adapted from kernel-cloud migrations/0011_energy_and_trust_flag.sql,
-- 0015_kb_energy_ratings.sql, and 0020_tighten_rating_usage_basis_check.sql;
-- source paths: ../kernel-cloud/migrations/0011_energy_and_trust_flag.sql,
-- ../kernel-cloud/migrations/0015_kb_energy_ratings.sql,
-- ../kernel-cloud/migrations/0020_tighten_rating_usage_basis_check.sql.
--
-- Keep each evidence tier as its own row. An EPA exact row and an OEM fallback
-- may coexist without merging values or provenance. Unreviewed rows can be
-- persisted for curation but are excluded by the store's read query.

CREATE TABLE IF NOT EXISTS energy_rating_catalog (
  rating_id          BIGSERIAL PRIMARY KEY,
  brand              TEXT NOT NULL CHECK (length(trim(brand)) > 0),
  model              TEXT NOT NULL CHECK (length(trim(model)) > 0),
  category           TEXT NOT NULL CHECK (length(trim(category)) > 0),
  source_kind        TEXT NOT NULL CHECK (source_kind IN ('epa_exact', 'oem_exact', 'oem_family')),
  annual_kwh         NUMERIC CHECK (annual_kwh >= 0),
  rated_watts        NUMERIC CHECK (rated_watts >= 0),
  standby_watts      NUMERIC CHECK (standby_watts >= 0),
  kwh_per_cycle      NUMERIC CHECK (kwh_per_cycle >= 0),
  rating_usage_basis TEXT CHECK (
    rating_usage_basis ~ '^(hours_per_day:(([1-9]|1[0-9]|2[0-3])(\.[0-9]+)?|24(\.0+)?)|cycles_per_year:[1-9][0-9]*(\.[0-9]+)?|continuous)$'
  ),
  source_ref         TEXT NOT NULL CHECK (length(trim(source_ref)) > 0),
  source_row_id      TEXT,
  source_fetched_at  TIMESTAMPTZ,
  contract_version   TEXT NOT NULL CHECK (length(trim(contract_version)) > 0),
  reviewed           BOOLEAN NOT NULL DEFAULT FALSE,
  reviewed_at        TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (annual_kwh IS NOT NULL OR rated_watts IS NOT NULL OR standby_watts IS NOT NULL OR kwh_per_cycle IS NOT NULL),
  CHECK (NOT reviewed OR reviewed_at IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS energy_rating_catalog_identity_unique
  ON energy_rating_catalog (lower(brand), lower(model), source_kind);

CREATE INDEX IF NOT EXISTS energy_rating_catalog_reviewed_model_idx
  ON energy_rating_catalog (lower(brand), lower(model), category)
  WHERE reviewed = TRUE;

-- Reviewed evidence is immutable through ordinary updates. A reviewer may
-- approve an unreviewed candidate (false -> true); changing the rating or
-- clearing its reviewed state requires a separately designed correction path.
CREATE OR REPLACE FUNCTION protect_reviewed_energy_rating() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' AND NEW.reviewed THEN
    RAISE EXCEPTION 'energy ratings must be inserted as unreviewed candidates';
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.reviewed AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'reviewed energy rating is immutable';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS energy_rating_catalog_reviewed_immutable ON energy_rating_catalog;
CREATE TRIGGER energy_rating_catalog_reviewed_immutable
  BEFORE INSERT OR UPDATE ON energy_rating_catalog
  FOR EACH ROW EXECUTE FUNCTION protect_reviewed_energy_rating();
