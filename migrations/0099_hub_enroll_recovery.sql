-- Unclaimed hubs prove their device key to obtain a recovery enrollment nonce.
ALTER TABLE hub_enrollment_nonces
  ADD COLUMN purpose text NOT NULL DEFAULT 'operator' CHECK (purpose IN ('operator','unclaimed_recovery')),
  ADD COLUMN ownership_epoch bigint,
  ADD CONSTRAINT hub_enrollment_nonces_recovery_epoch_check CHECK ((purpose='unclaimed_recovery') = (ownership_epoch IS NOT NULL));
CREATE INDEX hub_enrollment_nonces_open_idx ON hub_enrollment_nonces(hub_id) WHERE consumed_at IS NULL;
