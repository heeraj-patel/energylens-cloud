-- Private login throttles. Keys are HMAC-SHA256 digests of normalized email
-- and trusted source address; raw identifiers are never stored here.
CREATE TABLE auth_login_rate_limits (
  scope text NOT NULL CHECK (scope IN ('account_hour','source_hour')),
  key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
  window_start timestamptz NOT NULL,
  attempts integer NOT NULL CHECK (attempts BETWEEN 1 AND 1000000),
  PRIMARY KEY (scope,key_hash)
);
CREATE INDEX auth_login_rate_window_idx ON auth_login_rate_limits(window_start);
