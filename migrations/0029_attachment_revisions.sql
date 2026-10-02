-- Attachment rows are effective intervals; closing one is a versioned change.
-- Immutable snapshots preserve the original open state and each close.
ALTER TABLE attachments ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);
ALTER TABLE attachments ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE attachments ADD CONSTRAINT attachments_identity_revision_fk
  FOREIGN KEY (appliance_id,identity_revision) REFERENCES appliance_revisions(appliance_id,version);
CREATE TABLE attachment_revisions (
  attachment_id uuid NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
  version bigint NOT NULL CHECK (version > 0),
  home_id uuid NOT NULL REFERENCES homes(id),
  snapshot jsonb NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (attachment_id,version)
);
INSERT INTO attachment_revisions(attachment_id,version,home_id,snapshot)
SELECT a.id,1,a.home_id,jsonb_build_object(
  'id',a.id,'home_id',a.home_id,'sensor_id',a.sensor_id,
  'appliance_id',a.appliance_id,'identity_revision',a.identity_revision,
  'started_at',a.started_at,'ended_at',a.ended_at,'version',1)
FROM attachments a;
CREATE FUNCTION reject_attachment_revision_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'attachment revisions are immutable'; END $$;
CREATE TRIGGER attachment_revisions_immutable BEFORE UPDATE ON attachment_revisions
FOR EACH ROW EXECUTE FUNCTION reject_attachment_revision_change();
CREATE TABLE attachment_request_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  operation text NOT NULL,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_user_id,home_id,operation,key)
);
