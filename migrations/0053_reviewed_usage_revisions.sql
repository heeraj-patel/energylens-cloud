-- A signed revision closes one active reviewed fact. Historical fact and
-- publication rows remain immutable and retain their original provenance.
ALTER TABLE reviewed_usage_publications DROP CONSTRAINT reviewed_usage_publications_fact_count_check;
ALTER TABLE reviewed_usage_publications ADD CONSTRAINT reviewed_usage_publications_fact_count_check CHECK (fact_count >= 0);
DROP INDEX reviewed_usage_facts_one_identity_idx;

CREATE TABLE reviewed_usage_fact_revisions (
  id uuid PRIMARY KEY,
  publication_sha256 text NOT NULL REFERENCES reviewed_usage_publications(payload_sha256),
  prior_fact_id uuid NOT NULL UNIQUE REFERENCES reviewed_usage_facts(id),
  replacement_fact_id uuid UNIQUE REFERENCES reviewed_usage_facts(id),
  action text NOT NULL CHECK (action IN ('supersede','retract')),
  reason text NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 1000),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((action='supersede' AND replacement_fact_id IS NOT NULL AND replacement_fact_id<>prior_fact_id)
      OR (action='retract' AND replacement_fact_id IS NULL))
);
CREATE TRIGGER reviewed_usage_fact_revisions_immutable BEFORE UPDATE OR DELETE ON reviewed_usage_fact_revisions
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();

CREATE VIEW active_reviewed_usage_facts AS
SELECT f.* FROM reviewed_usage_facts f
WHERE NOT EXISTS (SELECT 1 FROM reviewed_usage_fact_revisions r WHERE r.prior_fact_id=f.id);

CREATE FUNCTION invalidate_reviewed_usage_dependents() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
    SELECT e.id,'dependency_changed' FROM estimate_runs e
    WHERE e.dependency_snapshot->>'reviewed_usage_fact_id'=NEW.prior_fact_id::text
    ON CONFLICT (run_id) DO NOTHING;
  UPDATE dishwasher_cycle_evidence c
    SET invalidated_at=now(),invalidation_reason='reviewed_fact_changed'
    WHERE c.fact_id=NEW.prior_fact_id AND c.invalidated_at IS NULL;
  RETURN NEW;
END $$;
CREATE TRIGGER reviewed_usage_revision_invalidates AFTER INSERT ON reviewed_usage_fact_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_reviewed_usage_dependents();
