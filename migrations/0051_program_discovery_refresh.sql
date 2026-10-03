-- Private owner requests point to a coalesced, location-only discovery job.
-- No owner, home, appliance, or idempotency metadata is stored on public runs.
CREATE TABLE program_discovery_refresh_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  country_code char(2) NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
  postal_code text NOT NULL CHECK (length(postal_code) BETWEEN 1 AND 16),
  category text NOT NULL CHECK (length(category) BETWEEN 1 AND 80),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','complete','partial','failed')),
  run_id uuid REFERENCES program_discovery_runs(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  finished_at timestamptz,
  failure_code text,
  CHECK ((state IN ('pending','running')) = (finished_at IS NULL))
);
CREATE UNIQUE INDEX program_discovery_refresh_active_scope_idx
 ON program_discovery_refresh_jobs(country_code,postal_code,category)
 WHERE state IN ('pending','running');
CREATE INDEX program_discovery_refresh_queue_idx ON program_discovery_refresh_jobs(created_at,id)
 WHERE state IN ('pending','running');

CREATE TABLE program_discovery_refresh_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  appliance_id uuid NOT NULL REFERENCES appliances(id),
  location_revision integer NOT NULL CHECK (location_revision > 0),
  country_code char(2) NOT NULL,
  postal_code text NOT NULL,
  category text NOT NULL,
  idempotency_key uuid NOT NULL,
  job_id uuid NOT NULL REFERENCES program_discovery_refresh_jobs(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(owner_user_id,home_id,idempotency_key)
);
CREATE INDEX program_discovery_refresh_owner_idx ON program_discovery_refresh_requests(owner_user_id,home_id,created_at DESC);
