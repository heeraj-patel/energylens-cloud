-- Offline OEDI ZIP leads have independent dataset provenance and cannot prove
-- household service or supply a reviewed program source.
ALTER TABLE program_discovery_runs
  ADD COLUMN resolution_provenance jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK (jsonb_typeof(resolution_provenance) = 'object');

ALTER TABLE program_discovery_coverage
  DROP CONSTRAINT program_discovery_coverage_provider_role_check;
ALTER TABLE program_discovery_coverage
  ADD CONSTRAINT program_discovery_coverage_provider_role_check
  CHECK (provider_role IN ('electric_service_candidate','electric_distributor','electric_retailer','gas_distributor','gas_retailer','other_fuel'));
