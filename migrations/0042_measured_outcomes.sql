-- Manual actions and their source context. Outcomes are read from current
-- measured rollups; a changed/corrected rollup must not leave a stale claim.
CREATE TABLE interventions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  appliance_id uuid NOT NULL,
  attachment_id uuid NOT NULL,
  rule_id text NOT NULL CHECK (length(rule_id) BETWEEN 1 AND 100),
  occurred_at timestamptz NOT NULL,
  comparable_use boolean NOT NULL,
  comparable_weather boolean NOT NULL,
  confounders text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id),
  FOREIGN KEY (attachment_id) REFERENCES attachments(id),
  CHECK (date_trunc('hour',occurred_at)=occurred_at),
  CHECK (cardinality(confounders)<=8),
  CHECK (confounders <@ ARRAY['occupancy_changed','use_changed','weather_changed','equipment_changed','other','unknown']::text[])
);
CREATE INDEX interventions_home_created_idx ON interventions(home_id,created_at DESC,id);
CREATE TABLE intervention_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  intervention_id uuid NOT NULL REFERENCES interventions(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(owner_user_id,home_id,key)
);
