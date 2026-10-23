-- Purge may delete a tombstoned home's revision periods and electric utility rows; other changes stay rejected.
CREATE FUNCTION reject_tariff_revision_period_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (SELECT 1 FROM tariff_revisions r JOIN homes h ON h.id=r.home_id WHERE r.id=OLD.tariff_revision_id AND h.deleted_at IS NOT NULL) THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'rate periods are immutable';
END $$;
DROP TRIGGER tariff_revision_period_immutable ON tariff_revision_periods;
CREATE TRIGGER tariff_revision_period_immutable BEFORE UPDATE OR DELETE ON tariff_revision_periods
  FOR EACH ROW EXECUTE FUNCTION reject_tariff_revision_period_change();
CREATE OR REPLACE FUNCTION reject_home_electric_utility_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (SELECT 1 FROM homes WHERE id=OLD.home_id AND deleted_at IS NOT NULL) THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'home electric utility rows are append-only';
END $$;
