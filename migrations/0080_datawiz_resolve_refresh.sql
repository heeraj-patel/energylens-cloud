-- Re-resolution reasons, owner refresh attempts and the Datawiz source fingerprint.
ALTER TABLE datawiz_resolve_queue DROP CONSTRAINT datawiz_resolve_queue_reason_check;
ALTER TABLE datawiz_resolve_queue ADD CONSTRAINT datawiz_resolve_queue_reason_check
  CHECK (reason IN ('usage_answer','admin','owner_refresh','low_tier','stale','source_config','catalog_change','schema'));
CREATE TABLE datawiz_resolve_attempts (
  appliance_id uuid NOT NULL,
  home_id uuid NOT NULL,
  reason text NOT NULL,
  queued_at timestamptz NOT NULL,
  PRIMARY KEY (appliance_id, queued_at)
);
CREATE INDEX datawiz_resolve_attempts_home_idx ON datawiz_resolve_attempts(home_id, queued_at);
CREATE TABLE datawiz_source_state (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  fingerprint text NOT NULL,
  updated_at timestamptz NOT NULL
);
