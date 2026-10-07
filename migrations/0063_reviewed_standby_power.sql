-- Reviewed standby reference power remains distinct from operating energy and
-- from any home-specific idle duration or measured waste.
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_measurement_kind_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_unit_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_included_energy_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_check1;
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_measurement_kind_check
  CHECK (measurement_kind IN ('cycle_energy','hourly_energy','standby_power'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_unit_check
  CHECK ((fuel='electricity' AND
           ((measurement_kind='cycle_energy' AND unit='kWh/cycle') OR
            (measurement_kind='hourly_energy' AND unit='kWh/hour') OR
            (measurement_kind='standby_power' AND unit='W' AND match_quality='confirmed_exact')))
      OR (fuel='natural_gas' AND category='dryer' AND match_quality='confirmed_exact'
          AND measurement_kind='cycle_energy' AND unit='therm/cycle'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_included_energy_check
  CHECK ((fuel='electricity' AND measurement_kind<>'standby_power'
          AND included_energy @> '{"status":"documented","components":["appliance_electricity"]}'::jsonb)
      OR (fuel='electricity' AND measurement_kind='standby_power'
          AND included_energy->>'status'='documented'
          AND included_energy->'components'='["standby_electricity"]'::jsonb)
      OR (fuel='natural_gas' AND included_energy->>'status'='documented'
          AND included_energy->'components'='["appliance_gas"]'::jsonb));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_mode_check
  CHECK ((measurement_kind='cycle_energy' AND cycle_definition IS NOT NULL AND operation_mode IS NULL)
      OR (measurement_kind IN ('hourly_energy','standby_power')
          AND operation_mode IS NOT NULL AND length(trim(operation_mode))>0 AND cycle_definition IS NULL));
