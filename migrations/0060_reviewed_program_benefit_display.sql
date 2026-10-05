-- A displayable benefit is a separate immutable, source-cited review field.
-- Earlier revisions have no reviewed display text and cannot be served.
ALTER TABLE public_program_revisions
  ADD COLUMN benefit_display text
  CHECK (benefit_display IS NULL OR (
    length(benefit_display) BETWEEN 1 AND 160
    AND benefit_display = btrim(benefit_display)
    AND benefit_display !~ '[[:cntrl:]]'
  ));

CREATE OR REPLACE VIEW current_verified_program_revisions AS
SELECT r.* FROM public_program_revisions r
JOIN public_programs p ON p.id = r.program_id
JOIN program_sources s ON s.id = p.source_id
JOIN program_source_snapshots snap ON snap.id = r.source_snapshot_id AND snap.source_id = s.id
JOIN program_source_snapshot_content content ON content.snapshot_id = snap.id
WHERE r.revision = (SELECT max(newer.revision) FROM public_program_revisions newer WHERE newer.program_id = r.program_id)
  AND r.lifecycle = 'reviewed'
  AND r.administrator_kind IS NOT NULL
  AND r.benefit_display IS NOT NULL
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
    SELECT 1 FROM unnest(ARRAY['administrator_kind','title','status','benefit_kind','benefit_display','benefit_terms','eligibility_terms','stacking_terms',
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
