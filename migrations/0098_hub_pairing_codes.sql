-- Expiring Cloud rendezvous for unclaimed Hubs. Local enrollment remains a
-- separate 256-bit, one-use capability; only encrypted material is retained.
CREATE TABLE hub_pairing_codes (
  hub_id uuid PRIMARY KEY REFERENCES hubs(id) ON DELETE CASCADE,
  publication_id uuid NOT NULL UNIQUE,
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  publisher_cert_serial text NOT NULL REFERENCES hub_certificates(serial) ON DELETE CASCADE,
  code_hash bytea NOT NULL UNIQUE CHECK (octet_length(code_hash)=32),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  enrollment_ciphertext bytea NOT NULL,
  https_origin text NOT NULL,
  certificate_sha256 text NOT NULL CHECK (certificate_sha256 ~ '^[0-9a-f]{64}$'),
  published_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > published_at),
  resolved_owner_user_id uuid REFERENCES users(id) ON DELETE CASCADE,
  resolved_home_id uuid REFERENCES homes(id) ON DELETE CASCADE,
  resolved_session_id text,
  resolved_client_id uuid,
  resolve_operation_id uuid,
  CHECK ((resolved_owner_user_id IS NULL AND resolved_home_id IS NULL AND
          resolved_session_id IS NULL AND resolved_client_id IS NULL AND resolve_operation_id IS NULL)
      OR (resolved_owner_user_id IS NOT NULL AND resolved_home_id IS NOT NULL AND
          resolved_session_id IS NOT NULL AND resolved_client_id IS NOT NULL AND resolve_operation_id IS NOT NULL))
);

-- Retain publication IDs through their original lifetime so a delayed retry
-- cannot replace a newer code with an old, previously superseded code.
CREATE TABLE hub_pairing_publication_ids (
  publication_id uuid PRIMARY KEY,
  hub_id uuid NOT NULL REFERENCES hubs(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL
);

CREATE TABLE hub_pairing_rate_limits (
  scope text NOT NULL CHECK (scope IN ('owner', 'source', 'global')),
  key_hash bytea NOT NULL CHECK (octet_length(key_hash)=32),
  window_start timestamptz NOT NULL,
  attempts integer NOT NULL CHECK (attempts > 0),
  PRIMARY KEY(scope,key_hash)
);
