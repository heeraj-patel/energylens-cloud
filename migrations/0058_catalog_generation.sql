-- Catalog generations are allocated in the same transaction as a publication.
-- Backfill existing immutable publications in a stable order before serving reads.
CREATE TABLE catalog_generation (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  generation bigint NOT NULL CHECK (generation >= 0)
);
INSERT INTO catalog_generation(singleton, generation)
SELECT true, count(*) FROM catalog_publications;

ALTER TABLE catalog_publications ADD COLUMN generation bigint;
-- The immutable-row trigger predates this metadata. Migration execution is
-- transactional and ALTER TABLE holds an exclusive lock throughout backfill.
DROP TRIGGER catalog_publications_immutable ON catalog_publications;
WITH numbered AS (
  SELECT payload_sha256, row_number() OVER (ORDER BY imported_at, payload_sha256) AS generation
  FROM catalog_publications
)
UPDATE catalog_publications p SET generation = numbered.generation
FROM numbered WHERE p.payload_sha256 = numbered.payload_sha256;
CREATE TRIGGER catalog_publications_immutable BEFORE UPDATE OR DELETE ON catalog_publications
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
ALTER TABLE catalog_publications ALTER COLUMN generation SET NOT NULL;
ALTER TABLE catalog_publications ADD CONSTRAINT catalog_publications_generation_positive CHECK (generation > 0);
ALTER TABLE catalog_publications ADD CONSTRAINT catalog_publications_generation_unique UNIQUE (generation);

CREATE FUNCTION allocate_catalog_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE catalog_generation SET generation = generation + 1
  WHERE singleton = true RETURNING generation INTO NEW.generation;
  IF NEW.generation IS NULL THEN
    RAISE EXCEPTION 'catalog generation state missing';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER catalog_publication_generation BEFORE INSERT ON catalog_publications
  FOR EACH ROW EXECUTE FUNCTION allocate_catalog_generation();
