-- A manual report preview is a private, immutable, short-lived snapshot.
-- No recipient or delivery state is stored here; sending is a separate flow.
CREATE TABLE report_previews (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL,
  home_version bigint NOT NULL CHECK (home_version > 0),
  idempotency_key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  schema_version text NOT NULL CHECK (schema_version = 'manual-preview/v1'),
  payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  FOREIGN KEY (owner_user_id,home_id) REFERENCES homes(owner_user_id,id) ON DELETE CASCADE,
  UNIQUE (owner_user_id,home_id,idempotency_key),
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours')
);
CREATE INDEX report_previews_owner_home_idx ON report_previews(owner_user_id,home_id,created_at DESC);

-- Dependencies are local IDs and revisions only. They allow a later read to
-- mark the preview stale without mutating the original payload.
CREATE TABLE report_preview_items (
  preview_id uuid NOT NULL REFERENCES report_previews(id) ON DELETE CASCADE,
  appliance_id uuid NOT NULL,
  appliance_version bigint NOT NULL CHECK (appliance_version > 0),
  estimate_run_id uuid REFERENCES estimate_runs(id),
  PRIMARY KEY (preview_id,appliance_id)
);
CREATE FUNCTION reject_report_preview_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'report previews are immutable'; END $$;
CREATE TRIGGER report_previews_immutable BEFORE UPDATE ON report_previews
  FOR EACH ROW EXECUTE FUNCTION reject_report_preview_update();
CREATE TRIGGER report_preview_items_immutable BEFORE UPDATE ON report_preview_items
  FOR EACH ROW EXECUTE FUNCTION reject_report_preview_update();
