SELECT jsonb_pretty(jsonb_build_object(
  'jobs', (SELECT coalesce(jsonb_agg(q.doc ORDER BY q.doc->>'country', q.doc->>'postal', q.doc->>'category'), '[]'::jsonb) FROM (
    SELECT jsonb_build_object('country', j.country_code, 'postal', j.postal_code, 'category', j.category, 'state', j.state,
      'failure_code', j.failure_code, 'finished', j.finished_at IS NOT NULL,
      'run', CASE WHEN r.id IS NULL THEN NULL ELSE jsonb_build_object('state', r.state, 'state_code', r.state_code,
        'county_name', r.county_name, 'city_name', r.city_name, 'category', r.category, 'limitations', r.limitations,
        'provenance', r.resolution_provenance, 'finished', r.finished_at IS NOT NULL,
        'coverage', (SELECT coalesce(jsonb_agg(jsonb_build_object('kind', c.scope_kind, 'key', c.scope_key, 'name', c.scope_name,
            'role', c.provider_role, 'source_key', s.source_key, 'status', c.status, 'failure_code', c.failure_code,
            'checked', c.checked_at IS NOT NULL, 'retry_after', c.retry_after IS NOT NULL, 'notes', c.notes)
            ORDER BY c.scope_kind, c.scope_key, c.provider_role NULLS FIRST), '[]'::jsonb)
          FROM program_discovery_coverage c LEFT JOIN program_sources s ON s.id = c.source_id WHERE c.run_id = r.id)) END) AS doc
    FROM program_discovery_refresh_jobs j LEFT JOIN program_discovery_runs r ON r.id = j.run_id) q),
  'run_count', (SELECT count(*) FROM program_discovery_runs),
  'snapshots', (SELECT coalesce(jsonb_agg(jsonb_build_object('source_key', s.source_key, 'source_url', p.source_url,
      'sha256', p.content_sha256, 'type', p.content_type, 'bytes', p.content_bytes, 'parser_version', p.parser_version,
      'ref', p.snapshot_ref, 'content_sha256', encode(sha256(c.content), 'hex'))
      ORDER BY s.source_key, p.content_sha256), '[]'::jsonb)
    FROM program_source_snapshots p JOIN program_sources s ON s.id = p.source_id
    JOIN program_source_snapshot_content c ON c.snapshot_id = p.id)))
