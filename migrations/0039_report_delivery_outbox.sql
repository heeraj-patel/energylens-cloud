-- A user-approved, manually queued delivery request. No scheduled producer or
-- email provider is installed by this migration. Deleting a home or preview
-- removes recipient data with it.
CREATE TABLE report_delivery_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL,
  preview_id uuid NOT NULL REFERENCES report_previews(id) ON DELETE CASCADE,
  idempotency_key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  recipient_hash bytea NOT NULL CHECK (octet_length(recipient_hash) = 32),
  recipients text[] NOT NULL CHECK (cardinality(recipients) BETWEEN 1 AND 5),
  approved_snapshot_sha256 text NOT NULL CHECK (approved_snapshot_sha256 ~ '^[0-9a-f]{64}$'),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','canceled')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
  last_error_code text,
  created_at timestamptz NOT NULL DEFAULT now(),
  sent_at timestamptz,
  FOREIGN KEY (owner_user_id,home_id) REFERENCES homes(owner_user_id,id) ON DELETE CASCADE,
  UNIQUE (owner_user_id,home_id,idempotency_key),
  UNIQUE (owner_user_id,preview_id,recipient_hash),
  CHECK ((state = 'sent') = (sent_at IS NOT NULL))
);
CREATE INDEX report_delivery_pending_idx ON report_delivery_requests(created_at,id) WHERE state='pending';
CREATE INDEX report_delivery_owner_rate_idx ON report_delivery_requests(owner_user_id,created_at DESC);
CREATE FUNCTION reject_report_delivery_identity_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.owner_user_id,NEW.home_id,NEW.preview_id,NEW.idempotency_key,NEW.request_hash,
      NEW.recipient_hash,NEW.recipients,NEW.approved_snapshot_sha256,NEW.created_at)
     IS DISTINCT FROM
     (OLD.owner_user_id,OLD.home_id,OLD.preview_id,OLD.idempotency_key,OLD.request_hash,
      OLD.recipient_hash,OLD.recipients,OLD.approved_snapshot_sha256,OLD.created_at) THEN
    RAISE EXCEPTION 'report delivery identity is immutable';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER report_delivery_identity_immutable BEFORE UPDATE ON report_delivery_requests
  FOR EACH ROW EXECUTE FUNCTION reject_report_delivery_identity_change();
