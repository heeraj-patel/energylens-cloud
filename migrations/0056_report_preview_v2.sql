-- Keep existing approved v1 snapshots readable while allowing the additive
-- measured/recommendation/gas report representation.
ALTER TABLE report_previews DROP CONSTRAINT report_previews_schema_version_check;
ALTER TABLE report_previews ADD CONSTRAINT report_previews_schema_version_check
  CHECK (schema_version IN ('manual-preview/v1','manual-preview/v2'));

-- V2 previews may include corrected meter rollups and current actions. A
-- change to either evidence stream revokes pending delivery of that snapshot.
CREATE TABLE report_preview_invalidations (
  preview_id uuid PRIMARY KEY REFERENCES report_previews(id) ON DELETE CASCADE,
  reason text NOT NULL CHECK (reason IN ('meter_evidence_changed','recommendation_evidence_changed')),
  invalidated_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION invalidate_report_previews_for_home() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected_home uuid;
BEGIN
  IF TG_OP='DELETE' THEN affected_home := OLD.home_id;
  ELSE affected_home := NEW.home_id;
  END IF;
  INSERT INTO report_preview_invalidations(preview_id,reason)
    SELECT p.id,CASE WHEN TG_TABLE_NAME='energy_daily' THEN 'meter_evidence_changed' ELSE 'recommendation_evidence_changed' END
    FROM report_previews p WHERE p.home_id=affected_home AND p.schema_version='manual-preview/v2' AND p.expires_at>now()
    ON CONFLICT(preview_id) DO NOTHING;
  IF TG_OP='DELETE' THEN RETURN OLD;
  ELSE RETURN NEW;
  END IF;
END $$;
CREATE TRIGGER report_meter_rollup_changed AFTER INSERT OR UPDATE OR DELETE ON energy_daily
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_home();
CREATE TRIGGER report_question_answer_changed AFTER INSERT ON question_answers
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_home();
CREATE TRIGGER report_recommendation_event_changed AFTER INSERT ON recommendation_events
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_home();
CREATE TRIGGER report_tariff_changed AFTER INSERT ON tariff_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_home();

CREATE FUNCTION invalidate_report_previews_for_public_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO report_preview_invalidations(preview_id,reason)
    SELECT p.id,'recommendation_evidence_changed' FROM report_previews p
    WHERE p.schema_version='manual-preview/v2' AND p.expires_at>now()
    ON CONFLICT(preview_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER report_usage_fact_changed AFTER INSERT ON reviewed_usage_fact_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_public_fact();
CREATE TRIGGER report_catalog_fact_changed AFTER INSERT ON catalog_fact_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_public_fact();
