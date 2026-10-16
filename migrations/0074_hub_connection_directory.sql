-- Short-lived, Hub-published LAN connection hints for the current owner.
-- The hints never authorize a customer request; the Hub still checks the
-- current Cloud owner session on every claimed-home request.
CREATE TABLE hub_connections (
  hub_id uuid PRIMARY KEY REFERENCES hubs(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  publisher_cert_serial text NOT NULL REFERENCES hub_certificates(serial) ON DELETE CASCADE,
  https_origin text NOT NULL CHECK (length(https_origin) BETWEEN 1 AND 128),
  certificate_sha256 text NOT NULL CHECK (certificate_sha256 ~ '^[0-9a-f]{64}$'),
  published_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > published_at)
);
