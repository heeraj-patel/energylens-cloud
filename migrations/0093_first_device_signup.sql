-- Email signup can activate a credential without claiming mailbox verification.
ALTER TABLE auth_password_credentials
  DROP CONSTRAINT auth_password_credentials_mode_check,
  ADD CONSTRAINT auth_password_credentials_mode_check
    CHECK (
      (credential_kind = 'email' AND email_normalized IS NOT NULL AND username_normalized IS NULL
        AND (activated_at IS NULL OR verified_at IS NULL))
      OR
      (credential_kind = 'username_local' AND email_normalized IS NULL AND username_normalized IS NOT NULL
        AND activated_at IS NOT NULL AND verified_at IS NULL)
    );

ALTER TABLE homes ADD COLUMN first_device_added_at timestamptz;

-- Existing persisted appliances are real rows; demo inventory is derived at read time.
UPDATE homes h SET first_device_added_at = first_device.created_at
FROM (
  SELECT home_id, min(created_at) AS created_at FROM appliances GROUP BY home_id
) first_device
WHERE first_device.home_id = h.id;
