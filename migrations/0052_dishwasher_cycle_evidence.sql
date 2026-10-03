-- A bounded owner-private snapshot of an explicitly tagged dishwasher cycle.
-- Raw observations remain subject to the normal 30-day detail retention.
CREATE TABLE dishwasher_cycle_evidence (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  appliance_id uuid NOT NULL,
  appliance_version bigint NOT NULL CHECK (appliance_version > 0),
  attachment_id uuid NOT NULL,
  sensor_id uuid NOT NULL,
  hub_id uuid NOT NULL,
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  start_at timestamptz NOT NULL,
  end_at timestamptz NOT NULL,
  dedicated_no_other_loads boolean NOT NULL CHECK (dedicated_no_other_loads),
  fact_id uuid NOT NULL,
  fact_publication_sha256 char(64) NOT NULL CHECK (fact_publication_sha256 ~ '^[0-9a-f]{64}$'),
  fact_review_sha256 char(64) NOT NULL CHECK (fact_review_sha256 ~ '^[0-9a-f]{64}$'),
  fact_source_sha256 char(64) NOT NULL CHECK (fact_source_sha256 ~ '^[0-9a-f]{64}$'),
  confirmed_test_basis text NOT NULL CHECK (length(confirmed_test_basis) BETWEEN 1 AND 500),
  confirmed_cycle_definition text NOT NULL CHECK (length(confirmed_cycle_definition) BETWEEN 1 AND 500),
  measured_kwh double precision NOT NULL CHECK (measured_kwh >= 0 AND measured_kwh < 'Infinity'::float8),
  reference_kwh_per_cycle double precision NOT NULL CHECK (reference_kwh_per_cycle > 0 AND reference_kwh_per_cycle < 'Infinity'::float8),
  interval_refs jsonb NOT NULL CHECK (jsonb_typeof(interval_refs)='array' AND jsonb_array_length(interval_refs)>0),
  idempotency_key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  created_at timestamptz NOT NULL DEFAULT now(),
  invalidated_at timestamptz,
  invalidation_reason text,
  expires_at timestamptz NOT NULL DEFAULT (now()+interval '24 months'),
  UNIQUE(owner_user_id,home_id,idempotency_key),
  CHECK (end_at>start_at AND end_at-start_at<=interval '24 hours'),
  CHECK ((invalidated_at IS NULL)=(invalidation_reason IS NULL))
);
CREATE INDEX dishwasher_cycle_evidence_owner_idx ON dishwasher_cycle_evidence(owner_user_id,home_id,appliance_id,created_at DESC);
CREATE INDEX dishwasher_cycle_evidence_expiry_idx ON dishwasher_cycle_evidence(expires_at);

CREATE FUNCTION invalidate_dishwasher_cycle_for_interval() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e ingest_events%ROWTYPE;
BEGIN
  SELECT * INTO e FROM ingest_events WHERE hub_id=NEW.hub_id AND ownership_epoch=NEW.ownership_epoch AND event_id=NEW.event_id AND revision=NEW.revision;
  UPDATE dishwasher_cycle_evidence c SET invalidated_at=now(),invalidation_reason='meter_evidence_changed'
  WHERE c.invalidated_at IS NULL AND c.sensor_id=e.sensor_id AND c.hub_id=e.hub_id AND c.ownership_epoch=e.ownership_epoch
    AND ((NEW.start_at<c.end_at AND NEW.end_at>c.start_at)
      OR c.interval_refs @> jsonb_build_array(jsonb_build_object('event_id',NEW.event_id::text)));
  RETURN NEW;
END $$;
CREATE TRIGGER dishwasher_cycle_interval_changed AFTER INSERT OR UPDATE ON energy_observations
  FOR EACH ROW WHEN (NEW.kind='energy_interval') EXECUTE FUNCTION invalidate_dishwasher_cycle_for_interval();

CREATE FUNCTION invalidate_dishwasher_cycle_for_gap() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e ingest_events%ROWTYPE;
BEGIN
  SELECT * INTO e FROM ingest_events WHERE hub_id=NEW.hub_id AND ownership_epoch=NEW.ownership_epoch AND event_id=NEW.event_id AND revision=NEW.revision;
  UPDATE dishwasher_cycle_evidence c SET invalidated_at=now(),invalidation_reason='coverage_changed'
  WHERE c.invalidated_at IS NULL AND c.sensor_id=e.sensor_id AND c.hub_id=e.hub_id AND c.ownership_epoch=e.ownership_epoch
    AND NEW.first_at<c.end_at AND NEW.last_at>c.start_at;
  RETURN NEW;
END $$;
CREATE TRIGGER dishwasher_cycle_gap_changed AFTER INSERT OR UPDATE ON coverage_gaps
  FOR EACH ROW EXECUTE FUNCTION invalidate_dishwasher_cycle_for_gap();

CREATE FUNCTION invalidate_dishwasher_cycle_for_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE dishwasher_cycle_evidence SET invalidated_at=now(),invalidation_reason='attachment_changed'
    WHERE invalidated_at IS NULL AND attachment_id=OLD.id;
  RETURN OLD;
END $$;
CREATE TRIGGER dishwasher_cycle_attachment_changed AFTER UPDATE OR DELETE ON attachments
  FOR EACH ROW EXECUTE FUNCTION invalidate_dishwasher_cycle_for_attachment();

CREATE FUNCTION invalidate_dishwasher_cycle_for_appliance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE dishwasher_cycle_evidence SET invalidated_at=now(),invalidation_reason='appliance_changed'
    WHERE invalidated_at IS NULL AND appliance_id=OLD.id;
  RETURN OLD;
END $$;
CREATE TRIGGER dishwasher_cycle_appliance_changed AFTER UPDATE OF version,identity,archived_at OR DELETE ON appliances
  FOR EACH ROW EXECUTE FUNCTION invalidate_dishwasher_cycle_for_appliance();
