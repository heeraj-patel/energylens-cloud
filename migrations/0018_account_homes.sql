-- Account and home foundation. Identity is the OIDC issuer/subject pair;
-- email is mutable profile data and never an account lookup key.
CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email text,
  email_verified boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS auth_identities (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issuer text NOT NULL CHECK (length(issuer) > 0),
  subject text NOT NULL CHECK (length(subject) > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (issuer, subject)
);

CREATE TABLE IF NOT EXISTS homes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(trim(name)) > 0),
  timezone text NOT NULL CHECK (length(timezone) > 2),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  UNIQUE (owner_user_id, id)
);
CREATE INDEX IF NOT EXISTS homes_owner_active_idx ON homes(owner_user_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS home_location_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  revision bigint NOT NULL CHECK (revision > 0),
  country_code char(2) NOT NULL,
  postal_code text NOT NULL CHECK (length(trim(postal_code)) > 0),
  city text,
  county text,
  precision text NOT NULL CHECK (precision IN ('postal_code','city','address')),
  source text NOT NULL CHECK (source IN ('user_entered','user_confirmed','geocoded')),
  recorded_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (home_id, revision)
);

CREATE TABLE IF NOT EXISTS account_home_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  operation text NOT NULL,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  response_json jsonb NOT NULL,
  response_status integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_user_id, operation, key)
);
