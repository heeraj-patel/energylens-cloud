-- A password reset invalidates previously issued local access JWTs immediately.
ALTER TABLE auth_password_credentials ADD COLUMN access_version bigint NOT NULL DEFAULT 1 CHECK (access_version > 0);
