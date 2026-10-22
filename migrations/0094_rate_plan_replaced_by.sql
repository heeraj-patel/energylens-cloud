-- The official plan that replaces a URDB plan while current; Datawiz sets and clears it every run, Cloud hides such plans.
ALTER TABLE utility_rate_plans
  ADD COLUMN replaced_by_plan_key text CHECK (replaced_by_plan_key ~ '^official:[a-z0-9:._-]{1,120}$');
CREATE OR REPLACE FUNCTION guard_utility_rate_plan_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'utility rate plans are immutable'; END IF;
  IF to_jsonb(NEW) - 'checked_at' - 'superseded_at' - 'replaced_by_plan_key'
     IS DISTINCT FROM to_jsonb(OLD) - 'checked_at' - 'superseded_at' - 'replaced_by_plan_key' THEN
    RAISE EXCEPTION 'utility rate plans change only checked_at, superseded_at and replaced_by_plan_key';
  END IF;
  RETURN NEW;
END $$;
