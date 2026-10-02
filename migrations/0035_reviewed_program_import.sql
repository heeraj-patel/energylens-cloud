-- Keep the exact private bytes behind every reviewed program source snapshot.
-- The existing snapshot row alone records a digest and locator, not content.
-- Re-fetching unchanged terms must create a fresh reviewable snapshot. One
-- source artifact can also support several separately reviewed programs.
DO $$
DECLARE key_name text;
BEGIN
  SELECT conname INTO key_name FROM pg_constraint
  WHERE conrelid = 'program_source_snapshots'::regclass AND contype = 'u'
    AND conkey = ARRAY[2,3,4]::smallint[];
  IF key_name IS NULL THEN RAISE EXCEPTION 'program snapshot uniqueness constraint missing'; END IF;
  EXECUTE format('ALTER TABLE program_source_snapshots DROP CONSTRAINT %I', key_name);
END $$;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE TABLE program_source_snapshot_content (
  snapshot_id uuid PRIMARY KEY REFERENCES program_source_snapshots(id),
  content bytea NOT NULL CHECK (octet_length(content) > 0),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER program_source_snapshot_content_immutable BEFORE UPDATE OR DELETE ON program_source_snapshot_content
  FOR EACH ROW EXECUTE FUNCTION reject_public_program_evidence_change();

-- A reviewer must cite the actual immutable source for every customer-facing
-- term group. A newer candidate or an unresolved conflict still suppresses
-- the previous reviewed revision.
CREATE OR REPLACE VIEW current_verified_program_revisions AS
SELECT r.* FROM public_program_revisions r
JOIN public_programs p ON p.id = r.program_id
JOIN program_sources s ON s.id = p.source_id
JOIN program_source_snapshots snap ON snap.id = r.source_snapshot_id AND snap.source_id = s.id
JOIN program_source_snapshot_content content ON content.snapshot_id = snap.id
WHERE r.revision = (SELECT max(newer.revision) FROM public_program_revisions newer WHERE newer.program_id = r.program_id)
  AND r.lifecycle = 'reviewed'
  AND r.reviewed_at <= now()
  AND r.review_expires_at > now()
  AND s.source_kind IN ('government','administrator','utility','manufacturer')
  AND s.terms_reviewed_at <= now()
  AND s.terms_review_expires_at > now()
  AND snap.fetched_at <= r.reviewed_at
  AND snap.fetched_at > now() - interval '90 days'
  AND encode(public.digest(content.content, 'sha256'), 'hex') = snap.content_sha256
  AND octet_length(content.content) = snap.content_bytes
  AND NOT EXISTS (
    SELECT 1 FROM unnest(ARRAY['title','status','benefit_kind','benefit_terms','eligibility_terms','stacking_terms',
      CASE WHEN r.purchase_from IS NOT NULL THEN 'purchase_from' END,
      CASE WHEN r.purchase_to IS NOT NULL THEN 'purchase_to' END,
      CASE WHEN r.installation_from IS NOT NULL THEN 'installation_from' END,
      CASE WHEN r.installation_to IS NOT NULL THEN 'installation_to' END,
      CASE WHEN r.reservation_from IS NOT NULL THEN 'reservation_from' END,
      CASE WHEN r.reservation_to IS NOT NULL THEN 'reservation_to' END,
      CASE WHEN r.application_from IS NOT NULL THEN 'application_from' END,
      CASE WHEN r.application_to IS NOT NULL THEN 'application_to' END]) required(field_path)
    WHERE required.field_path IS NOT NULL AND NOT EXISTS (
      SELECT 1 FROM public_program_evidence e
      WHERE e.revision_id = r.id AND e.field_path = required.field_path AND e.snapshot_id = snap.id
    )
  )
  AND NOT EXISTS (
    SELECT 1 FROM public_program_conflicts c
    LEFT JOIN public_program_conflict_resolutions cr ON cr.conflict_id = c.id
    WHERE c.revision_id = r.id AND (cr.conflict_id IS NULL OR cr.controlling_snapshot_id <> snap.id)
  );
