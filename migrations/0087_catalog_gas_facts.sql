-- Catalog facts also carry gas-side ratings and electric power for gas appliances. Each kind has one unit
-- and a value range; annual_energy stays electric kWh and now allows a reported 0.
ALTER TABLE catalog_fact_revisions DROP CONSTRAINT catalog_fact_revisions_kind_check;
ALTER TABLE catalog_fact_revisions DROP CONSTRAINT catalog_fact_revisions_unit_check;
ALTER TABLE catalog_fact_revisions DROP CONSTRAINT catalog_fact_revisions_value_check;
ALTER TABLE catalog_fact_revisions ADD CONSTRAINT catalog_fact_revisions_kind_unit_check
  CHECK ((kind, unit) IN (
    ('annual_energy','kWh'),
    ('annual_fuel','therm'),
    ('annual_fuel','gallon'),
    ('combined_energy_factor','lb/kWh'),
    ('uniform_energy_factor','ratio'),
    ('afue','percent'),
    ('standby_power','W'),
    ('off_mode_power','W'),
    ('auxiliary_power','W'),
    ('burner_input','Btu/h')));
ALTER TABLE catalog_fact_revisions ADD CONSTRAINT catalog_fact_revisions_value_check
  CHECK ((kind IN ('annual_energy','standby_power','off_mode_power') AND value >= 0)
      OR (kind IN ('annual_fuel','combined_energy_factor','auxiliary_power','burner_input') AND value > 0)
      OR (kind = 'uniform_energy_factor' AND value > 0 AND value <= 5)
      OR (kind = 'afue' AND value > 0 AND value <= 100));
