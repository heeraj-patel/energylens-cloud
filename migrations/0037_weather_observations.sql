-- Shared coarse-location daily weather cache. Rows are immutable so evidence
-- dependencies can name the exact provider retrieval used in an answer.
CREATE TABLE weather_observations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 80),
  cell_key text NOT NULL CHECK (length(cell_key) BETWEEN 1 AND 80),
  local_date date NOT NULL,
  timezone text NOT NULL CHECK (length(timezone) > 2),
  mean_temperature_c numeric(5,2) NOT NULL CHECK (mean_temperature_c BETWEEN -100 AND 70),
  source_revision text NOT NULL CHECK (length(source_revision) BETWEEN 1 AND 160),
  retrieved_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > retrieved_at),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX weather_observations_lookup_idx ON weather_observations(provider,cell_key,local_date,timezone,retrieved_at DESC,id DESC);

CREATE FUNCTION reject_weather_observation_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'weather observations are immutable';
END;
$$;
CREATE TRIGGER weather_observations_immutable BEFORE UPDATE OR DELETE ON weather_observations
FOR EACH ROW EXECUTE FUNCTION reject_weather_observation_change();
