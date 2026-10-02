-- Home-owned, immutable marginal tariff revisions. Fixed monthly charges are
-- stored separately and must never be added to marginal savings calculations.
CREATE TABLE tariff_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id),
  revision bigint NOT NULL CHECK (revision > 0),
  fuel text NOT NULL CHECK (fuel IN ('electricity','natural_gas','propane','fuel_oil')),
  rate numeric(18,8) NOT NULL CHECK (rate >= 0),
  rate_unit text NOT NULL CHECK (rate_unit IN ('kWh','therm','gallon')),
  currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  rate_kind text NOT NULL CHECK (rate_kind IN ('user_tariff','official_tariff','regional_average')),
  source_ref text NOT NULL CHECK (length(trim(source_ref)) BETWEEN 1 AND 2048),
  source_as_of timestamptz NOT NULL,
  is_fallback boolean NOT NULL,
  fixed_charge numeric(18,4) CHECK (fixed_charge >= 0),
  effective_from timestamptz NOT NULL,
  effective_to timestamptz,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((fuel='electricity' AND rate_unit='kWh') OR
         (fuel='natural_gas' AND rate_unit='therm') OR
         (fuel IN ('propane','fuel_oil') AND rate_unit='gallon')),
  CHECK (effective_to IS NULL OR effective_to > effective_from),
  CHECK (rate_kind <> 'regional_average' OR is_fallback),
  UNIQUE (home_id,fuel,revision)
);
CREATE INDEX tariff_revisions_home_recent_idx ON tariff_revisions(home_id,fuel,revision DESC);
CREATE TABLE tariff_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_user_id,home_id,key)
);
CREATE FUNCTION reject_tariff_revision_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'tariff revisions are immutable'; END $$;
CREATE TRIGGER tariff_revision_immutable BEFORE UPDATE OR DELETE ON tariff_revisions
  FOR EACH ROW EXECUTE FUNCTION reject_tariff_revision_change();
