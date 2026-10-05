-- Signed changes close one active annual fact while preserving all source rows.
ALTER TABLE catalog_publications DROP CONSTRAINT catalog_publications_product_count_check;
ALTER TABLE catalog_publications ADD CONSTRAINT catalog_publications_product_count_check CHECK (product_count >= 0);
ALTER TABLE catalog_publications DROP CONSTRAINT catalog_publications_fact_count_check;
ALTER TABLE catalog_publications ADD CONSTRAINT catalog_publications_fact_count_check CHECK (fact_count >= 0);

CREATE TABLE catalog_fact_changes (
  id uuid PRIMARY KEY,
  publication_sha256 text NOT NULL REFERENCES catalog_publications(payload_sha256),
  prior_fact_id uuid NOT NULL UNIQUE REFERENCES catalog_fact_revisions(id),
  replacement_fact_id uuid UNIQUE REFERENCES catalog_fact_revisions(id),
  action text NOT NULL CHECK (action IN ('supersede','retract')),
  reason text NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 1000),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((action='supersede' AND replacement_fact_id IS NOT NULL AND replacement_fact_id<>prior_fact_id)
      OR (action='retract' AND replacement_fact_id IS NULL))
);
CREATE TRIGGER catalog_fact_changes_immutable BEFORE UPDATE OR DELETE ON catalog_fact_changes
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();

CREATE VIEW active_catalog_facts AS
SELECT f.* FROM catalog_fact_revisions f
WHERE NOT EXISTS (SELECT 1 FROM catalog_fact_changes c WHERE c.prior_fact_id=f.id);

CREATE FUNCTION invalidate_catalog_change_dependents() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
    SELECT e.id,'catalog_fact_changed' FROM estimate_runs e
    JOIN appliances a ON a.id=e.appliance_id
    JOIN catalog_fact_revisions f ON f.product_id=a.catalog_product_id
    WHERE f.id=NEW.prior_fact_id
    ON CONFLICT (run_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER catalog_change_invalidates AFTER INSERT ON catalog_fact_changes
  FOR EACH ROW EXECUTE FUNCTION invalidate_catalog_change_dependents();
CREATE TRIGGER report_catalog_change_invalidates AFTER INSERT ON catalog_fact_changes
  FOR EACH ROW EXECUTE FUNCTION invalidate_report_previews_for_public_fact();
