-- Inactive recovery challenges and private HMAC-only request throttles.
CREATE TABLE auth_password_reset_tokens (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  user_id uuid NOT NULL REFERENCES auth_password_credentials(user_id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at),
  CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);
CREATE INDEX auth_password_reset_user_idx ON auth_password_reset_tokens(user_id,created_at DESC);

CREATE TABLE auth_password_reset_rate_limits (
  scope text NOT NULL CHECK (scope IN ('email_day','source_hour')),
  key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
  window_start timestamptz NOT NULL,
  attempts integer NOT NULL CHECK (attempts BETWEEN 1 AND 1000000),
  PRIMARY KEY (scope,key_hash)
);
CREATE INDEX auth_password_reset_rate_window_idx ON auth_password_reset_rate_limits(window_start);
