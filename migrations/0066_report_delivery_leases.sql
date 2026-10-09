ALTER TABLE report_delivery_requests
  DROP CONSTRAINT report_delivery_requests_state_check,
  ADD CONSTRAINT report_delivery_requests_state_check CHECK (state IN ('pending','leased','sent','canceled')),
  ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN lease_token uuid,
  ADD COLUMN lease_expires_at timestamptz,
  ADD CONSTRAINT report_delivery_lease_fields_check CHECK (
    (state = 'leased') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
  );

DROP INDEX report_delivery_pending_idx;
CREATE INDEX report_delivery_due_idx ON report_delivery_requests(next_attempt_at,created_at,id)
  WHERE state='pending';
CREATE INDEX report_delivery_expired_lease_idx ON report_delivery_requests(lease_expires_at,created_at,id)
  WHERE state='leased';
