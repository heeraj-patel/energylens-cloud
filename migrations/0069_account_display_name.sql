-- Customer-managed profile data; authentication continues to use issuer/subject.
ALTER TABLE users
  ADD COLUMN display_name text CHECK (display_name IS NULL OR (length(display_name) BETWEEN 1 AND 120 AND display_name = trim(display_name))),
  ADD COLUMN profile_version bigint NOT NULL DEFAULT 1 CHECK (profile_version > 0);
