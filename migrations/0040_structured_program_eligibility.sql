-- Operator-reviewed, source-cited geography and eligibility projection.
-- Existing reviewed revisions remain valid in the public registry but are not
-- eligible for customer reads until a structured projection is reviewed.
CREATE TABLE reviewed_program_applicability (
  revision_id uuid PRIMARY KEY REFERENCES public_program_revisions(id),
  country_code char(2) NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
  postal_codes text[] NOT NULL CHECK (cardinality(postal_codes) BETWEEN 1 AND 10000
    AND array_to_string(postal_codes, ',') ~ '^[0-9]{5}(,[0-9]{5})*$'),
  category text NOT NULL CHECK (length(trim(category)) > 0),
  fuel text NOT NULL CHECK (length(trim(fuel)) > 0),
  provider_key text,
  provider_role text CHECK (provider_role IN ('electric_distributor','electric_retailer','gas_distributor','gas_retailer','other_fuel')),
  requires_income boolean NOT NULL,
  requires_address boolean NOT NULL,
  additional_conditions boolean NOT NULL,
  CHECK ((provider_key IS NULL) = (provider_role IS NULL)),
  CHECK (provider_key IS NULL OR length(trim(provider_key)) > 0)
);
CREATE TRIGGER reviewed_program_applicability_immutable BEFORE UPDATE OR DELETE ON reviewed_program_applicability
  FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();
