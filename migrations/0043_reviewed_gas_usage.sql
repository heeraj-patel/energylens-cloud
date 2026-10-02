-- Add only exact natural-gas dryer cycle evidence. A therm basis cannot be
-- derived from the electrical circuit requirements of a gas dryer.
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_fuel_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_included_energy_check;
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_fuel_check
  CHECK (fuel IN ('electricity','natural_gas'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_unit_check
  CHECK ((fuel='electricity' AND
           ((measurement_kind='cycle_energy' AND unit='kWh/cycle') OR
            (measurement_kind='hourly_energy' AND unit='kWh/hour')))
      OR (fuel='natural_gas' AND category='dryer' AND match_quality='confirmed_exact'
          AND measurement_kind='cycle_energy' AND unit='therm/cycle'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_included_energy_check
  CHECK ((fuel='electricity' AND included_energy @> '{"status":"documented","components":["appliance_electricity"]}'::jsonb)
      OR (fuel='natural_gas' AND included_energy->>'status'='documented'
          AND included_energy->'components'='["appliance_gas"]'::jsonb));
