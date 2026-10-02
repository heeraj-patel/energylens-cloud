-- UTC rollups are per sensor and explicit attachment, never home totals.
-- An hourly/daily value may allocate a measured interval by elapsed time;
-- allocation_method distinguishes that from an exact bucket interval.
CREATE TABLE energy_hourly (
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch>0),
  sensor_id uuid NOT NULL REFERENCES sensor_endpoints(id),
  attachment_id uuid NOT NULL REFERENCES attachments(id),
  bucket_start timestamptz NOT NULL,
  energy_wh numeric(24,12),
  covered_seconds numeric(24,12),
  interval_count integer NOT NULL CHECK (interval_count>=0),
  status text NOT NULL CHECK (status IN ('complete','no_intervals','overlap_conflict')),
  allocation_method text NOT NULL CHECK (allocation_method IN ('exact','duration_proportional','none')),
  source_refs jsonb NOT NULL CHECK (jsonb_typeof(source_refs)='array'),
  computed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start),
  CHECK ((status='complete' AND energy_wh IS NOT NULL AND covered_seconds IS NOT NULL)
      OR (status<>'complete' AND energy_wh IS NULL AND covered_seconds IS NULL))
);
CREATE TABLE energy_daily (LIKE energy_hourly INCLUDING ALL);
ALTER TABLE energy_daily ADD FOREIGN KEY (home_id) REFERENCES homes(id) ON DELETE CASCADE;
ALTER TABLE energy_daily ADD FOREIGN KEY (hub_id) REFERENCES hubs(id);
ALTER TABLE energy_daily ADD FOREIGN KEY (sensor_id) REFERENCES sensor_endpoints(id);
ALTER TABLE energy_daily ADD FOREIGN KEY (attachment_id) REFERENCES attachments(id);
CREATE TABLE coverage_hourly (
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch>0),
  sensor_id uuid NOT NULL REFERENCES sensor_endpoints(id),
  bucket_start timestamptz NOT NULL,
  gap_overlap_count integer NOT NULL CHECK (gap_overlap_count>=0),
  dropped_count_started bigint NOT NULL CHECK (dropped_count_started>=0),
  source_refs jsonb NOT NULL CHECK (jsonb_typeof(source_refs)='array'),
  computed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(home_id,hub_id,ownership_epoch,sensor_id,bucket_start)
);
CREATE TABLE coverage_daily (LIKE coverage_hourly INCLUDING ALL);
ALTER TABLE coverage_daily ADD FOREIGN KEY (home_id) REFERENCES homes(id) ON DELETE CASCADE;
ALTER TABLE coverage_daily ADD FOREIGN KEY (hub_id) REFERENCES hubs(id);
ALTER TABLE coverage_daily ADD FOREIGN KEY (sensor_id) REFERENCES sensor_endpoints(id);
-- Power samples remain samples: these statistics never imply energy or coverage.
CREATE TABLE power_hourly (
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch>0),
  sensor_id uuid NOT NULL REFERENCES sensor_endpoints(id),
  bucket_start timestamptz NOT NULL,
  sample_count integer NOT NULL CHECK (sample_count>=0),
  min_watts double precision,
  max_watts double precision,
  mean_watts double precision,
  source_refs jsonb NOT NULL CHECK (jsonb_typeof(source_refs)='array'),
  computed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(home_id,hub_id,ownership_epoch,sensor_id,bucket_start),
  CHECK ((sample_count=0 AND min_watts IS NULL AND max_watts IS NULL AND mean_watts IS NULL)
      OR (sample_count>0 AND min_watts IS NOT NULL AND max_watts IS NOT NULL AND mean_watts IS NOT NULL))
);
CREATE TABLE power_daily (LIKE power_hourly INCLUDING ALL);
ALTER TABLE power_daily ADD FOREIGN KEY (home_id) REFERENCES homes(id) ON DELETE CASCADE;
ALTER TABLE power_daily ADD FOREIGN KEY (hub_id) REFERENCES hubs(id);
ALTER TABLE power_daily ADD FOREIGN KEY (sensor_id) REFERENCES sensor_endpoints(id);
CREATE TABLE metered_rollup_checkpoint (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  last_cursor bigint NOT NULL DEFAULT 0 CHECK (last_cursor>=0),
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO metered_rollup_checkpoint(singleton,last_cursor) VALUES(true,0);
CREATE TABLE metered_rollup_dirty (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('energy_hour','energy_day','gap_hour','gap_day','power_hour','power_day')),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch>0),
  sensor_id uuid NOT NULL REFERENCES sensor_endpoints(id),
  attachment_id uuid REFERENCES attachments(id),
  bucket_start timestamptz NOT NULL,
  changed_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT metered_rollup_dirty_scope UNIQUE NULLS NOT DISTINCT
    (kind,home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start),
  CHECK ((kind IN ('energy_hour','energy_day') AND attachment_id IS NOT NULL)
      OR (kind IN ('gap_hour','gap_day','power_hour','power_day') AND attachment_id IS NULL))
);
