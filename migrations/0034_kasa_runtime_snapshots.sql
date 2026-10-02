-- Immutable Kasa relay-on runtime evidence. Today, past7, and past30 overlap.
-- No duration is measured energy or evidence that an attached load was active.
CREATE TABLE kasa_runtime_snapshots (
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  snapshot_id uuid NOT NULL,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  sensor_id uuid NOT NULL,
  attachment_id uuid,
  scope text NOT NULL CHECK (scope = 'single_plug'),
  window_label text NOT NULL CHECK (window_label IN ('today','past7','past30')),
  native_unit text NOT NULL CHECK (native_unit = 'minutes'),
  native_minutes bigint NOT NULL CHECK (native_minutes >= 0),
  duration_seconds bigint NOT NULL CHECK (duration_seconds = native_minutes * 60),
  reported_start timestamptz,
  reported_end timestamptz,
  device_region text,
  device_clock_valid boolean NOT NULL,
  observed_at timestamptz NOT NULL,
  method text NOT NULL CHECK (method IN ('get_device_usage','get_daystat')),
  transport text NOT NULL CHECK (transport IN ('kasa_http_klap','kasa_legacy_tcp')),
  firmware text,
  quality text NOT NULL CHECK (quality IN ('protocol_reported_clock_verified','protocol_reported_clock_unverified')),
  payload_hash bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
  accepted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (hub_id,ownership_epoch,snapshot_id),
  FOREIGN KEY (home_id,sensor_id) REFERENCES sensor_endpoints(home_id,id),
  FOREIGN KEY (attachment_id) REFERENCES attachments(id),
  CHECK ((reported_start IS NULL AND reported_end IS NULL) OR
         (reported_start IS NOT NULL AND reported_end IS NOT NULL AND reported_end > reported_start AND reported_end <= observed_at)),
  CHECK (attachment_id IS NULL OR (reported_start IS NOT NULL AND device_clock_valid AND quality = 'protocol_reported_clock_verified')),
  CHECK ((method = 'get_device_usage' AND transport = 'kasa_http_klap') OR
         (method = 'get_daystat' AND transport = 'kasa_legacy_tcp')),
  CHECK (quality = 'protocol_reported_clock_unverified' OR device_clock_valid)
);
CREATE INDEX kasa_runtime_home_page_idx ON kasa_runtime_snapshots(home_id,observed_at DESC,snapshot_id DESC);
CREATE FUNCTION reject_kasa_runtime_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'kasa runtime snapshots are immutable'; END $$;
CREATE TRIGGER kasa_runtime_immutable BEFORE UPDATE ON kasa_runtime_snapshots
FOR EACH ROW EXECUTE FUNCTION reject_kasa_runtime_change();
