-- Explicit country scope supports national household offers without inventing
-- a ZIP list. Earlier reviewed rows remain ZIP scoped.
ALTER TABLE reviewed_program_applicability
  DROP CONSTRAINT reviewed_program_applicability_postal_codes_check;
ALTER TABLE reviewed_program_applicability
  ADD COLUMN geography_scope text NOT NULL DEFAULT 'postal_code'
    CHECK (geography_scope IN ('postal_code','country')),
  ADD CONSTRAINT reviewed_program_applicability_geography_check
    CHECK ((geography_scope = 'country' AND cardinality(postal_codes) = 0)
      OR (geography_scope = 'postal_code' AND cardinality(postal_codes) BETWEEN 1 AND 10000
        AND array_to_string(postal_codes, ',') ~ '^[0-9]{5}(,[0-9]{5})*$')),
  ADD CONSTRAINT reviewed_program_applicability_home_check
    CHECK ((category = 'home') = (fuel = 'any'));
