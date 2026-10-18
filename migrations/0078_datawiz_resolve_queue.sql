-- Durable re-resolution requests from usage answers and administrator actions.
CREATE TABLE datawiz_resolve_queue (
  appliance_id uuid PRIMARY KEY,
  reason text NOT NULL CHECK (reason IN ('usage_answer','admin')),
  queued_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX datawiz_resolve_queue_queued_idx ON datawiz_resolve_queue(queued_at, appliance_id);
