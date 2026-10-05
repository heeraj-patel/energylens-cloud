-- STAGING ONLY. The name guard prevents accidental use in a production DB.
DO $$ BEGIN
  IF current_database() !~ '^energylens_program_staging_' THEN
    RAISE EXCEPTION 'DOE WAP packet may register only in an energylens_program_staging_ database';
  END IF;
END $$;

INSERT INTO program_sources (
  source_key,source_kind,authority_name,administrator_name,canonical_url,
  allowed_hosts,access_method,terms_url,terms_reviewed_at,
  terms_review_expires_at,terms_reviewed_by,parser_version,
  canonicalization_version,geography_coverage,category_coverage,
  refresh_interval,max_requests_per_run,max_response_bytes,enabled
) VALUES (
  'doe-wap-2026-staging','government','U.S. Department of Energy',
  'U.S. Department of Energy',
  'https://www.energy.gov/cmei/scep/wap/how-apply-weatherization-assistance',
  ARRAY['www.energy.gov'],'manual','https://www.energy.gov/web-policies',
  '2026-10-05T00:00:00Z','2026-10-13T00:00:00Z','staging-access-review',
  'manual-v1','v1','{}'::jsonb,'[]'::jsonb,interval '365 days',1,1000000,true
);
