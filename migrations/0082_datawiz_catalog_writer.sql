-- Datawiz writes catalog, usage, rated-input and scenario rows directly. KB publications keep their
-- signature columns; Datawiz publications carry a run id instead. Existing rows are not touched.
ALTER TABLE catalog_publications
  ADD COLUMN publisher text NOT NULL DEFAULT 'kb_import' CHECK (publisher IN ('kb_import','datawiz')),
  ADD COLUMN datawiz_run_id uuid REFERENCES datawiz_runs(id),
  ALTER COLUMN key_id DROP NOT NULL,
  ALTER COLUMN envelope_sha256 DROP NOT NULL,
  ADD CONSTRAINT catalog_publications_signature_required
    CHECK (publisher = 'datawiz' OR (key_id IS NOT NULL AND envelope_sha256 IS NOT NULL));
ALTER TABLE reviewed_usage_publications
  ADD COLUMN publisher text NOT NULL DEFAULT 'kb_import' CHECK (publisher IN ('kb_import','datawiz')),
  ADD COLUMN datawiz_run_id uuid REFERENCES datawiz_runs(id),
  ALTER COLUMN key_id DROP NOT NULL,
  ALTER COLUMN envelope_sha256 DROP NOT NULL,
  ADD CONSTRAINT reviewed_usage_publications_signature_required
    CHECK (publisher = 'datawiz' OR (key_id IS NOT NULL AND envelope_sha256 IS NOT NULL));
ALTER TABLE reviewed_rated_input_publications
  ADD COLUMN publisher text NOT NULL DEFAULT 'kb_import' CHECK (publisher IN ('kb_import','datawiz')),
  ADD COLUMN datawiz_run_id uuid REFERENCES datawiz_runs(id),
  ALTER COLUMN key_id DROP NOT NULL,
  ALTER COLUMN envelope_sha256 DROP NOT NULL,
  ADD CONSTRAINT reviewed_rated_input_publications_signature_required
    CHECK (publisher = 'datawiz' OR (key_id IS NOT NULL AND envelope_sha256 IS NOT NULL));
ALTER TABLE reviewed_action_benefit_publications
  ADD COLUMN publisher text NOT NULL DEFAULT 'kb_import' CHECK (publisher IN ('kb_import','datawiz')),
  ADD COLUMN datawiz_run_id uuid REFERENCES datawiz_runs(id),
  ALTER COLUMN key_id DROP NOT NULL,
  ALTER COLUMN envelope_sha256 DROP NOT NULL,
  ADD CONSTRAINT reviewed_action_benefit_publications_signature_required
    CHECK (publisher = 'datawiz' OR (key_id IS NOT NULL AND envelope_sha256 IS NOT NULL));

-- Evidence class and scope describe Datawiz facts; rows imported from KB keep both NULL.
ALTER TABLE catalog_fact_revisions
  ADD COLUMN evidence_class text CHECK (evidence_class IN ('official_registry_row','official_family_row','official_directory_family','oem_official_document','oem_search_document','manual_research')),
  ADD COLUMN scope text CHECK (scope IN ('exact','family'));
ALTER TABLE reviewed_usage_facts
  ADD COLUMN evidence_class text CHECK (evidence_class IN ('official_registry_row','official_family_row','official_directory_family','oem_official_document','oem_search_document','manual_research')),
  ADD COLUMN scope text CHECK (scope IN ('exact','family'));
ALTER TABLE reviewed_rated_input_facts
  ADD COLUMN evidence_class text CHECK (evidence_class IN ('official_registry_row','official_family_row','official_directory_family','oem_official_document','oem_search_document','manual_research')),
  ADD COLUMN scope text CHECK (scope IN ('exact','family'));

ALTER TABLE catalog_fact_revisions DROP CONSTRAINT catalog_fact_revisions_source_kind_check;
ALTER TABLE catalog_fact_revisions ADD CONSTRAINT catalog_fact_revisions_source_kind_check
  CHECK (source_kind IN ('epa_exact','oem_exact','oem_family','other_official','epa_family','doe_ccd','oem_search','manual_research'));
ALTER TABLE catalog_fact_revisions DROP CONSTRAINT catalog_fact_revisions_source_hash_basis_check;
ALTER TABLE catalog_fact_revisions ADD CONSTRAINT catalog_fact_revisions_source_hash_basis_check
  CHECK (source_hash_basis IN ('verified_source_document','reviewed_row_manifest','source_row','manual_entry'));

-- The per-kind shape check (0033) gains the Datawiz kinds. Row kinds carry a dataset row and field;
-- search and manual kinds carry no dataset columns.
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_source_kind_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_check2;
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_source_kind_check
  CHECK (source_kind IN ('oem_pdf','epa_row','epa_family','doe_ccd','oem_search','manual_research'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_check2
  CHECK ((source_kind='oem_pdf' AND source_page_index IS NOT NULL AND source_page_index >= 0
          AND dataset_id IS NULL AND row_identifier IS NULL AND field_name IS NULL AND metadata_sha256 IS NULL)
      OR (source_kind IN ('epa_row','epa_family') AND source_page_index IS NULL AND dataset_id IS NOT NULL
          AND row_identifier IS NOT NULL AND field_name IS NOT NULL AND metadata_sha256 ~ '^[0-9a-f]{64}$')
      OR (source_kind='doe_ccd' AND source_page_index IS NULL AND dataset_id IS NOT NULL
          AND row_identifier IS NOT NULL AND field_name IS NOT NULL
          AND (metadata_sha256 IS NULL OR metadata_sha256 ~ '^[0-9a-f]{64}$'))
      OR (source_kind IN ('oem_search','manual_research') AND (source_page_index IS NULL OR source_page_index >= 0)
          AND dataset_id IS NULL AND row_identifier IS NULL AND field_name IS NULL AND metadata_sha256 IS NULL));

ALTER TABLE reviewed_rated_input_facts DROP CONSTRAINT reviewed_rated_input_facts_source_kind_check;
ALTER TABLE reviewed_rated_input_facts ADD CONSTRAINT reviewed_rated_input_facts_source_kind_check
  CHECK (source_kind IN ('oem_rated_input','oem_official_document','epa_family','doe_ccd','oem_search','manual_research'));

-- Research imports re-queue appliances with their own reason.
ALTER TABLE datawiz_resolve_queue DROP CONSTRAINT datawiz_resolve_queue_reason_check;
ALTER TABLE datawiz_resolve_queue ADD CONSTRAINT datawiz_resolve_queue_reason_check
  CHECK (reason IN ('usage_answer','admin','owner_refresh','low_tier','stale','source_config','catalog_change','schema','manual_research'));
