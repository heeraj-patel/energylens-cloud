-- Private owner actions. Source evidence is recomputed on read; an event is
-- never itself proof of a current benefit or a measured outcome.
CREATE TABLE recommendation_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  appliance_id uuid NOT NULL,
  rule_id text NOT NULL CHECK (length(rule_id) BETWEEN 1 AND 100),
  rule_version integer NOT NULL CHECK (rule_version > 0),
  state text NOT NULL CHECK (state IN ('accepted','snoozed','dismissed','completed')),
  snoozed_until timestamptz,
  idempotency_key uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id) ON DELETE CASCADE,
  UNIQUE(owner_user_id,home_id,idempotency_key),
  CHECK ((state='snoozed') = (snoozed_until IS NOT NULL))
);
CREATE INDEX recommendation_events_latest_idx ON recommendation_events(home_id,appliance_id,rule_id,created_at DESC,id DESC);
