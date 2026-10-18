-- Resolve route and one current Datawiz resolution per appliance.
ALTER TABLE datawiz_runs DROP CONSTRAINT datawiz_runs_route_check,
  ADD CONSTRAINT datawiz_runs_route_check
  CHECK (route IN ('device','zip','savings','efficiency','resolve'));
ALTER TABLE datawiz_observations DROP CONSTRAINT datawiz_observations_route_check,
  ADD CONSTRAINT datawiz_observations_route_check
  CHECK (route IN ('device','zip','savings','efficiency','resolve'));
ALTER TABLE datawiz_request_emissions DROP CONSTRAINT datawiz_request_emissions_route_check,
  ADD CONSTRAINT datawiz_request_emissions_route_check
  CHECK (route IN ('device','zip','savings','efficiency','resolve'));
CREATE TABLE datawiz_energy_resolutions (
  appliance_ref text PRIMARY KEY CHECK (length(btrim(appliance_ref)) > 0),
  request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
  record jsonb NOT NULL CHECK (jsonb_typeof(record) = 'object'),
  run_id uuid REFERENCES datawiz_runs(id) ON DELETE SET NULL,
  resolved_at timestamptz NOT NULL
);
