-- A durable in-database deletion journal. Operators must copy this journal to
-- independent durable storage and reapply it after restoring an older backup.
-- The user/home rows remain as disabled tombstones until the separate purge
-- job removes private dependent rows in dependency order.
ALTER TABLE users ADD COLUMN deleted_at timestamptz;
CREATE TABLE privacy_deletion_journal (
  sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('account','home')),
  object_id uuid NOT NULL,
  deleted_at timestamptz NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(kind,object_id)
);
CREATE INDEX privacy_deletion_journal_recorded_idx ON privacy_deletion_journal(sequence);

-- Immutable customer revisions may be removed only after the owning home has
-- been tombstoned. Public catalog and shared weather immutability is unchanged.
CREATE OR REPLACE FUNCTION reject_question_answer_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (SELECT 1 FROM homes WHERE id=OLD.home_id AND deleted_at IS NOT NULL) THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'question answer revisions are immutable';
END $$;
CREATE OR REPLACE FUNCTION reject_tariff_revision_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (SELECT 1 FROM homes WHERE id=OLD.home_id AND deleted_at IS NOT NULL) THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'tariff revisions are immutable';
END $$;
CREATE OR REPLACE FUNCTION reject_estimate_run_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (SELECT 1 FROM homes WHERE id=OLD.home_id AND deleted_at IS NOT NULL) THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'estimate runs are immutable';
END $$;
