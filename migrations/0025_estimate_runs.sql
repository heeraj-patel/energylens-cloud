-- Deterministic estimate snapshots are immutable. Dependency changes append
-- invalidation records; a later read creates a new run from current evidence.
CREATE TABLE estimate_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id),
  appliance_id uuid NOT NULL,
  appliance_version bigint NOT NULL CHECK (appliance_version > 0),
  period_start date NOT NULL,
  period_end date NOT NULL,
  timezone text NOT NULL,
  formula_version text NOT NULL,
  dependency_hash bytea NOT NULL CHECK (octet_length(dependency_hash)=32),
  dependency_snapshot jsonb NOT NULL,
  result_json jsonb NOT NULL,
  as_of timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id),
  CHECK (period_end > period_start),
  UNIQUE(home_id,appliance_id,period_start,period_end,dependency_hash)
);
CREATE INDEX estimate_runs_lookup_idx ON estimate_runs(home_id,appliance_id,period_start DESC,as_of DESC);
CREATE TABLE estimate_run_invalidations (
  run_id uuid PRIMARY KEY REFERENCES estimate_runs(id),
  reason text NOT NULL CHECK (reason IN ('appliance_changed','tariff_changed','catalog_fact_changed','period_changed','dependency_changed')),
  invalidated_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION invalidate_appliance_estimates() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
  SELECT id,'appliance_changed' FROM estimate_runs WHERE appliance_id=NEW.id
  ON CONFLICT (run_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER appliance_estimates_changed AFTER UPDATE ON appliances
  FOR EACH ROW EXECUTE FUNCTION invalidate_appliance_estimates();
CREATE FUNCTION invalidate_tariff_estimates() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
  SELECT id,'tariff_changed' FROM estimate_runs WHERE home_id=NEW.home_id
  ON CONFLICT (run_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER tariff_estimates_changed AFTER INSERT ON tariff_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_tariff_estimates();
CREATE FUNCTION invalidate_catalog_estimates() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
  SELECT e.id,'catalog_fact_changed' FROM estimate_runs e
    JOIN appliances a ON a.id=e.appliance_id
    WHERE a.catalog_product_id=NEW.product_id
  ON CONFLICT (run_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER catalog_estimates_changed AFTER INSERT ON catalog_fact_revisions
  FOR EACH ROW EXECUTE FUNCTION invalidate_catalog_estimates();
CREATE FUNCTION reject_estimate_run_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'estimate runs are immutable'; END $$;
CREATE TRIGGER estimate_runs_immutable BEFORE UPDATE OR DELETE ON estimate_runs
  FOR EACH ROW EXECUTE FUNCTION reject_estimate_run_change();
