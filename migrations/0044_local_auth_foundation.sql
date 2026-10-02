-- Inactive foundation for EnergyLens-owned credentials. No public route uses
-- these tables until verification, rate limiting, and rotation are complete.
CREATE TABLE auth_password_credentials (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  email_normalized text NOT NULL UNIQUE CHECK (
    email_normalized = lower(email_normalized) AND length(email_normalized) BETWEEN 3 AND 254
    AND email_normalized !~ '[[:space:]]'),
  password_hash text NOT NULL CHECK (length(password_hash) BETWEEN 50 AND 255),
  verified_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE auth_email_verification_tokens (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  user_id uuid NOT NULL REFERENCES auth_password_credentials(user_id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at),
  CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);
CREATE INDEX auth_email_verification_user_idx ON auth_email_verification_tokens(user_id,created_at DESC);

CREATE TABLE auth_refresh_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  family_id uuid NOT NULL,
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  rotated_to uuid REFERENCES auth_refresh_sessions(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at),
  CHECK (revoked_at IS NULL OR revoked_at >= created_at),
  CHECK (rotated_to IS NULL OR revoked_at IS NOT NULL)
);
CREATE INDEX auth_refresh_sessions_user_idx ON auth_refresh_sessions(user_id,created_at DESC);
CREATE INDEX auth_refresh_sessions_family_active_idx ON auth_refresh_sessions(family_id) WHERE revoked_at IS NULL;
