-- Direct requests are processed only by their initiating API call. A failed
-- or interrupted call never becomes a delayed worker send.
ALTER TABLE report_delivery_requests
  ADD COLUMN delivery_mode text NOT NULL DEFAULT 'queued'
    CHECK (delivery_mode IN ('queued', 'direct'));

ALTER TABLE report_delivery_requests
  DROP CONSTRAINT report_delivery_requests_state_check,
  ADD CONSTRAINT report_delivery_requests_state_check
    CHECK (state IN ('pending', 'leased', 'sent', 'canceled', 'unconfirmed'));

CREATE OR REPLACE FUNCTION reject_report_delivery_identity_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.owner_user_id,NEW.home_id,NEW.preview_id,NEW.idempotency_key,NEW.request_hash,
      NEW.recipient_hash,NEW.recipients,NEW.approved_snapshot_sha256,NEW.created_at,NEW.delivery_mode)
     IS DISTINCT FROM
     (OLD.owner_user_id,OLD.home_id,OLD.preview_id,OLD.idempotency_key,OLD.request_hash,
      OLD.recipient_hash,OLD.recipients,OLD.approved_snapshot_sha256,OLD.created_at,OLD.delivery_mode) THEN
    RAISE EXCEPTION 'report delivery identity is immutable';
  END IF;
  RETURN NEW;
END $$;
