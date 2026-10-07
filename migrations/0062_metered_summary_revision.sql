-- A transactionally advanced home revision makes keyset continuations detect
-- late rollups, corrections, and changes to the identity joins used by reads.
CREATE TABLE metered_summary_revision (
  home_id uuid PRIMARY KEY REFERENCES homes(id) ON DELETE CASCADE,
  revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0)
);

CREATE FUNCTION advance_metered_summary_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_home uuid; new_home uuid;
BEGIN
  IF TG_OP <> 'INSERT' THEN old_home := OLD.home_id; END IF;
  IF TG_OP <> 'DELETE' THEN new_home := NEW.home_id; END IF;
  IF old_home IS NOT NULL THEN
    INSERT INTO metered_summary_revision(home_id,revision) VALUES(old_home,1)
    ON CONFLICT (home_id) DO UPDATE SET revision=metered_summary_revision.revision+1;
  END IF;
  IF new_home IS NOT NULL AND new_home IS DISTINCT FROM old_home THEN
    INSERT INTO metered_summary_revision(home_id,revision) VALUES(new_home,1)
    ON CONFLICT (home_id) DO UPDATE SET revision=metered_summary_revision.revision+1;
  END IF;
  RETURN NULL;
END $$;

DO $$ DECLARE table_name text;
BEGIN
  FOREACH table_name IN ARRAY ARRAY['energy_hourly','energy_daily','power_hourly','power_daily','coverage_hourly','coverage_daily','sensor_endpoints','attachments'] LOOP
    EXECUTE format('CREATE TRIGGER metered_summary_revision_changed AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION advance_metered_summary_revision()',table_name);
  END LOOP;
END $$;
