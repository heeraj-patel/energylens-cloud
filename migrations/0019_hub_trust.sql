-- Only the migration/operator role may provision hub identities. No public key
-- registration route exists. Runtime roles need only read registered keys.
CREATE TABLE hubs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  local_hub_id text NOT NULL UNIQUE CHECK (length(local_hub_id) BETWEEN 7 AND 128 AND local_hub_id ~ '^elhub_[A-Za-z0-9_-]+$'),
  device_public_key bytea NOT NULL UNIQUE CHECK (octet_length(device_public_key)=32),
  home_id uuid REFERENCES homes(id),
  ownership_epoch bigint NOT NULL DEFAULT 1 CHECK (ownership_epoch>0),
  version bigint NOT NULL DEFAULT 1 CHECK (version>0),
  registered_at timestamptz NOT NULL DEFAULT now(),
  registered_by text NOT NULL CHECK (length(trim(registered_by))>0),
  revoked_at timestamptz,
  UNIQUE(home_id,id)
);
CREATE UNIQUE INDEX hubs_home_active_idx ON hubs(id,home_id) WHERE home_id IS NOT NULL;
CREATE TABLE hub_enrollment_nonces (
  nonce_hash bytea PRIMARY KEY CHECK (octet_length(nonce_hash)=32),
  hub_id uuid NOT NULL REFERENCES hubs(id),
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  csr_hash bytea CHECK (csr_hash IS NULL OR octet_length(csr_hash)=32),
  cert_serial text,
  CHECK ((consumed_at IS NULL AND csr_hash IS NULL AND cert_serial IS NULL) OR (consumed_at IS NOT NULL AND csr_hash IS NOT NULL AND cert_serial IS NOT NULL))
);
CREATE TABLE hub_claims (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  hub_id uuid NOT NULL REFERENCES hubs(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  owner_user_id uuid NOT NULL REFERENCES users(id),
  nonce bytea NOT NULL CHECK (octet_length(nonce)=32),
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX hub_claims_pending_idx ON hub_claims(hub_id,expires_at) WHERE consumed_at IS NULL;
CREATE TABLE hub_possession_proofs (
  proof_hash bytea PRIMARY KEY CHECK (octet_length(proof_hash)=32),
  claim_id uuid NOT NULL UNIQUE REFERENCES hub_claims(id),
  consumed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE hub_ownership_history (
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch>0),
  home_id uuid NOT NULL REFERENCES homes(id),
  owner_user_id uuid NOT NULL REFERENCES users(id),
  started_at timestamptz NOT NULL DEFAULT now(),
  ended_at timestamptz,
  PRIMARY KEY(hub_id,ownership_epoch)
);
CREATE UNIQUE INDEX hub_ownership_current_idx ON hub_ownership_history(hub_id) WHERE ended_at IS NULL;
CREATE TABLE hub_certificates (
  serial text PRIMARY KEY,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL,
  issued_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  certificate_pem text NOT NULL
);
CREATE INDEX hub_certificates_active_idx ON hub_certificates(hub_id) WHERE revoked_at IS NULL;
CREATE TABLE hub_request_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id),
  operation text NOT NULL,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb,
  response_status integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(owner_user_id,operation,key)
);
CREATE TABLE hub_certificate_renewals (
  old_serial text PRIMARY KEY REFERENCES hub_certificates(serial),
  csr_hash bytea NOT NULL CHECK (octet_length(csr_hash)=32),
  new_serial text NOT NULL UNIQUE REFERENCES hub_certificates(serial),
  created_at timestamptz NOT NULL DEFAULT now()
);
