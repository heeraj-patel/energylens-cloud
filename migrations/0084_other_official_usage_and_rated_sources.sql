-- Official registry lists outside EPA and DOE (the CEC archived appliance lists) write standby and
-- rated-input watts. Usage facts carry the dataset row and field like the other row kinds.
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_source_kind_check;
ALTER TABLE reviewed_usage_facts DROP CONSTRAINT reviewed_usage_facts_check2;
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_source_kind_check
  CHECK (source_kind IN ('oem_pdf','epa_row','epa_family','doe_ccd','other_official','oem_search','manual_research'));
ALTER TABLE reviewed_usage_facts ADD CONSTRAINT reviewed_usage_facts_check2
  CHECK ((source_kind='oem_pdf' AND source_page_index IS NOT NULL AND source_page_index >= 0
          AND dataset_id IS NULL AND row_identifier IS NULL AND field_name IS NULL AND metadata_sha256 IS NULL)
      OR (source_kind IN ('epa_row','epa_family') AND source_page_index IS NULL AND dataset_id IS NOT NULL
          AND row_identifier IS NOT NULL AND field_name IS NOT NULL AND metadata_sha256 ~ '^[0-9a-f]{64}$')
      OR (source_kind IN ('doe_ccd','other_official') AND source_page_index IS NULL AND dataset_id IS NOT NULL
          AND row_identifier IS NOT NULL AND field_name IS NOT NULL
          AND (metadata_sha256 IS NULL OR metadata_sha256 ~ '^[0-9a-f]{64}$'))
      OR (source_kind IN ('oem_search','manual_research') AND (source_page_index IS NULL OR source_page_index >= 0)
          AND dataset_id IS NULL AND row_identifier IS NULL AND field_name IS NULL AND metadata_sha256 IS NULL));

ALTER TABLE reviewed_rated_input_facts DROP CONSTRAINT reviewed_rated_input_facts_source_kind_check;
ALTER TABLE reviewed_rated_input_facts ADD CONSTRAINT reviewed_rated_input_facts_source_kind_check
  CHECK (source_kind IN ('oem_rated_input','oem_official_document','epa_family','doe_ccd','other_official',
                         'oem_search','manual_research'));
