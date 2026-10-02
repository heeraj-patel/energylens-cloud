-- Durable, epoch-scoped ingest. Batch replays remain after detailed history
-- expires so a delayed hub retry cannot write the same data twice.
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
CREATE TABLE attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  sensor_id uuid NOT NULL,
  appliance_id uuid NOT NULL,
  identity_revision bigint NOT NULL CHECK (identity_revision > 0),
  started_at timestamptz NOT NULL,
  ended_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (home_id,sensor_id) REFERENCES sensor_endpoints(home_id,id),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id),
  CHECK (ended_at IS NULL OR ended_at > started_at)
);
CREATE UNIQUE INDEX attachments_one_open_sensor_idx ON attachments(sensor_id) WHERE ended_at IS NULL;
ALTER TABLE attachments ADD CONSTRAINT attachments_no_sensor_overlap
  EXCLUDE USING gist (sensor_id WITH =,
    tstzrange(started_at, COALESCE(ended_at, 'infinity'::timestamptz), '[)') WITH &&);
CREATE TABLE ingest_batches (
  cursor bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  batch_id uuid NOT NULL,
  home_id uuid NOT NULL REFERENCES homes(id),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb,
  accepted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (hub_id,ownership_epoch,batch_id)
);
CREATE TABLE ingest_events (
  hub_id uuid NOT NULL,
  ownership_epoch bigint NOT NULL,
  event_id uuid NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  home_id uuid NOT NULL REFERENCES homes(id),
  sensor_id uuid NOT NULL,
  event_kind text NOT NULL CHECK (event_kind IN ('power_point','energy_interval','coverage_gap')),
  payload_hash bytea NOT NULL CHECK (octet_length(payload_hash)=32),
  first_batch_id uuid NOT NULL,
  accepted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (hub_id,ownership_epoch,event_id,revision),
  FOREIGN KEY (hub_id,ownership_epoch,first_batch_id) REFERENCES ingest_batches(hub_id,ownership_epoch,batch_id),
  FOREIGN KEY (home_id,sensor_id) REFERENCES sensor_endpoints(home_id,id)
);
CREATE INDEX ingest_events_sensor_idx ON ingest_events(sensor_id,accepted_at);
CREATE TABLE energy_observations (
  hub_id uuid NOT NULL,
  ownership_epoch bigint NOT NULL,
  event_id uuid NOT NULL,
  revision bigint NOT NULL,
  kind text NOT NULL CHECK (kind IN ('power_point','energy_interval')),
  observed_at timestamptz,
  start_at timestamptz,
  end_at timestamptz,
  watts double precision CHECK (watts >= 0 AND watts < 'Infinity'::float8),
  kwh_cumulative double precision CHECK (kwh_cumulative >= 0 AND kwh_cumulative < 'Infinity'::float8),
  energy_wh double precision CHECK (energy_wh >= 0 AND energy_wh < 'Infinity'::float8),
  covered_seconds integer CHECK (covered_seconds > 0),
  attachment_revision_id uuid REFERENCES attachments(id),
  fuel text,
  quality text,
  source text,
  source_method text,
  source_transport text,
  PRIMARY KEY (hub_id,ownership_epoch,event_id,revision),
  FOREIGN KEY (hub_id,ownership_epoch,event_id,revision) REFERENCES ingest_events(hub_id,ownership_epoch,event_id,revision),
  CHECK ((kind='power_point' AND observed_at IS NOT NULL AND watts IS NOT NULL AND start_at IS NULL AND end_at IS NULL AND energy_wh IS NULL AND covered_seconds IS NULL AND attachment_revision_id IS NULL)
      OR (kind='energy_interval' AND observed_at IS NULL AND watts IS NULL AND kwh_cumulative IS NULL AND start_at IS NOT NULL AND end_at > start_at AND energy_wh IS NOT NULL AND covered_seconds IS NOT NULL AND attachment_revision_id IS NOT NULL))
);
CREATE INDEX energy_observations_time_idx ON energy_observations(start_at,observed_at);
CREATE TABLE coverage_gaps (
  hub_id uuid NOT NULL,
  ownership_epoch bigint NOT NULL,
  event_id uuid NOT NULL,
  revision bigint NOT NULL,
  first_at timestamptz NOT NULL,
  last_at timestamptz NOT NULL,
  reason text NOT NULL CHECK (reason IN ('age_limit','byte_limit','counter_reset','offline','unknown')),
  dropped_count integer NOT NULL CHECK (dropped_count > 0),
  PRIMARY KEY (hub_id,ownership_epoch,event_id,revision),
  FOREIGN KEY (hub_id,ownership_epoch,event_id,revision) REFERENCES ingest_events(hub_id,ownership_epoch,event_id,revision),
  CHECK (last_at >= first_at)
);
