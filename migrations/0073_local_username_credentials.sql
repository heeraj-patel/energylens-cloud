-- Extend the existing credential table for explicitly selected local username accounts.
-- Existing email rows keep their identity, verification state and sessions.
ALTER TABLE auth_password_credentials
  ADD COLUMN credential_kind text NOT NULL DEFAULT 'email',
  ADD COLUMN username_normalized text,
  ADD COLUMN activated_at timestamptz;

ALTER TABLE auth_password_credentials
  ALTER COLUMN email_normalized DROP NOT NULL,
  ADD CONSTRAINT auth_password_credentials_kind_check
    CHECK (credential_kind IN ('email', 'username_local')),
  ADD CONSTRAINT auth_password_credentials_mode_check
    CHECK (
      (credential_kind = 'email' AND email_normalized IS NOT NULL AND username_normalized IS NULL AND activated_at IS NULL)
      OR
      (credential_kind = 'username_local' AND email_normalized IS NULL AND username_normalized IS NOT NULL
        AND activated_at IS NOT NULL AND verified_at IS NULL)
    ),
  ADD CONSTRAINT auth_password_credentials_username_check
    CHECK (username_normalized IS NULL OR
      (octet_length(username_normalized) = length(username_normalized)
       AND username_normalized COLLATE "C" ~ '^[a-z0-9][a-z0-9._-]{2,63}$'));

CREATE UNIQUE INDEX auth_password_credentials_username_local_unique
  ON auth_password_credentials (username_normalized)
  WHERE credential_kind = 'username_local';

ALTER TABLE auth_registration_rate_limits
  DROP CONSTRAINT auth_registration_rate_limits_scope_check,
  ADD CONSTRAINT auth_registration_rate_limits_scope_check
    CHECK (scope IN ('email_day', 'username_day', 'source_hour'));
