-- Portable Datawiz JSON runs and source-backed current observations.
CREATE TABLE datawiz_runs (
  id uuid PRIMARY KEY,
  route text NOT NULL CHECK (route IN ('device','zip','savings','efficiency')),
  request jsonb NOT NULL CHECK (jsonb_typeof(request)='object'),
  mode text NOT NULL CHECK (mode IN ('realtime','batch')),
  status text NOT NULL CHECK (status IN ('running','complete','partial','failed')),
  result jsonb CHECK (result IS NULL OR jsonb_typeof(result)='object'),
  started_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);
CREATE INDEX datawiz_runs_started_idx ON datawiz_runs(started_at DESC);

CREATE TABLE datawiz_observations (
  id uuid PRIMARY KEY,
  run_id uuid NOT NULL REFERENCES datawiz_runs(id) ON DELETE CASCADE,
  route text NOT NULL CHECK (route IN ('device','zip','savings','efficiency')),
  subject_key text NOT NULL CHECK (length(trim(subject_key))>0),
  field text NOT NULL CHECK (length(trim(field))>0),
  record_key text NOT NULL CHECK (length(trim(record_key))>0),
  value jsonb NOT NULL,
  source jsonb NOT NULL CHECK (jsonb_typeof(source)='object'),
  approval_status text NOT NULL CHECK (approval_status IN ('pending','approved','rejected')),
  collected_at timestamptz NOT NULL,
  UNIQUE (id,route,subject_key,field,record_key)
);
CREATE INDEX datawiz_observations_run_idx ON datawiz_observations(run_id);

CREATE TABLE datawiz_current (
  route text NOT NULL,
  subject_key text NOT NULL,
  field text NOT NULL,
  record_key text NOT NULL,
  observation_id uuid NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (route,subject_key,field,record_key),
  FOREIGN KEY (observation_id,route,subject_key,field,record_key)
    REFERENCES datawiz_observations(id,route,subject_key,field,record_key)
);

-- A durable marker for Cloud request production. Files are written before
-- markers, so a failed marker insert can only repeat work, never lose it.
CREATE TABLE datawiz_request_emissions (
  route text NOT NULL CHECK (route IN ('device','zip','savings','efficiency')),
  source_kind text NOT NULL,
  source_id uuid NOT NULL,
  source_version bigint NOT NULL CHECK (source_version>0),
  emitted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (route,source_kind,source_id,source_version)
);
