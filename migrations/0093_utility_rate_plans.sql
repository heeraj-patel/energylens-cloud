-- Utility rate plans and TOU adder periods collected by Datawiz (URDB bulk file and official tariffs); Cloud reads them.
-- Hours with no period row use the plan base; end_local < start_local wraps past midnight within each listed day.
CREATE TABLE utility_rate_plans (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  eiaid            text NOT NULL CHECK (eiaid ~ '^[0-9]+$'),
  utility_name     text NOT NULL,
  sector           text NOT NULL CHECK (sector = 'residential'),
  plan_key         text NOT NULL CHECK (plan_key ~ '^(urdb|official):[a-z0-9:._-]{1,120}$'),
  version          integer NOT NULL CHECK (version > 0),
  plan_name        text NOT NULL,
  variant          text,
  is_standard      boolean NOT NULL,
  base_tiers       jsonb CHECK (base_tiers IS NULL OR (jsonb_typeof(base_tiers) = 'array' AND jsonb_array_length(base_tiers) > 0 AND base_tiers->-1->'through_kwh' = 'null'::jsonb)),
  base_label       text NOT NULL,
  fixed_charge     numeric CHECK (fixed_charge >= 0),
  fixed_charge_per text CHECK (fixed_charge_per IN ('month','day')),
  currency         char(3) NOT NULL CHECK (currency = 'USD'),
  effective_from   date NOT NULL,
  effective_to     date,
  source_kind      text NOT NULL CHECK (source_kind IN ('official_tariff','urdb')),
  source_url       text NOT NULL,
  source_file_url  text NOT NULL,
  source_sha256    text NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  source_notes     jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(source_notes) = 'array'),
  retrieved_at     timestamptz NOT NULL,
  checked_at       timestamptz NOT NULL,
  superseded_at    timestamptz,
  loaded_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (plan_key, version),
  CHECK (effective_to IS NULL OR effective_to > effective_from),
  CHECK ((fixed_charge IS NULL) = (fixed_charge_per IS NULL)),
  CHECK ((source_kind = 'urdb') = (plan_key LIKE 'urdb:%'))
);
CREATE INDEX utility_rate_plans_current_eiaid_idx ON utility_rate_plans(eiaid) WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX utility_rate_plans_one_current_idx ON utility_rate_plans(plan_key) WHERE superseded_at IS NULL;
CREATE FUNCTION guard_utility_rate_plan_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'utility rate plans are immutable'; END IF;
  IF to_jsonb(NEW) - 'checked_at' - 'superseded_at' IS DISTINCT FROM to_jsonb(OLD) - 'checked_at' - 'superseded_at' THEN
    RAISE EXCEPTION 'utility rate plans change only checked_at and superseded_at';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER utility_rate_plan_guard BEFORE UPDATE OR DELETE ON utility_rate_plans
  FOR EACH ROW EXECUTE FUNCTION guard_utility_rate_plan_change();
CREATE TABLE utility_rate_plan_periods (
  plan_id     uuid NOT NULL REFERENCES utility_rate_plans(id),
  ordinal     smallint NOT NULL CHECK (ordinal > 0),
  name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 60),
  adder       numeric(18,8) NOT NULL CHECK (adder <> 0),
  days        text[] NOT NULL CHECK (cardinality(days) > 0 AND days <@ ARRAY['mon','tue','wed','thu','fri','sat','sun']),
  start_local time NOT NULL CHECK (start_local < '24:00'),
  end_local   time NOT NULL,
  months      smallint[] NOT NULL CHECK (cardinality(months) > 0 AND months <@ ARRAY[1,2,3,4,5,6,7,8,9,10,11,12]::smallint[]),
  PRIMARY KEY (plan_id, ordinal),
  CHECK (start_local <> end_local)
);
CREATE FUNCTION reject_rate_period_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'rate periods are immutable'; END $$;
CREATE TRIGGER utility_rate_plan_period_immutable BEFORE UPDATE OR DELETE ON utility_rate_plan_periods
  FOR EACH ROW EXECUTE FUNCTION reject_rate_period_change();
-- A home's revision may come from a plan; a user's pick and a default differ in reader rank.
ALTER TABLE tariff_revisions
  ADD COLUMN utility_rate_plan_id uuid REFERENCES utility_rate_plans(id),
  ADD COLUMN plan_chosen_by text CHECK (plan_chosen_by IN ('default','user')),
  ADD CONSTRAINT tariff_revisions_plan_choice_check CHECK ((utility_rate_plan_id IS NULL) = (plan_chosen_by IS NULL));
CREATE TABLE tariff_revision_periods (
  tariff_revision_id uuid NOT NULL REFERENCES tariff_revisions(id),
  ordinal            smallint NOT NULL CHECK (ordinal > 0),
  name               text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 60),
  adder              numeric(18,8) NOT NULL CHECK (adder <> 0),
  days               text[] NOT NULL CHECK (cardinality(days) > 0 AND days <@ ARRAY['mon','tue','wed','thu','fri','sat','sun']),
  start_local        time NOT NULL CHECK (start_local < '24:00'),
  end_local          time NOT NULL,
  months             smallint[] NOT NULL CHECK (cardinality(months) > 0 AND months <@ ARRAY[1,2,3,4,5,6,7,8,9,10,11,12]::smallint[]),
  PRIMARY KEY (tariff_revision_id, ordinal),
  CHECK (start_local <> end_local)
);
CREATE TRIGGER tariff_revision_period_immutable BEFORE UPDATE OR DELETE ON tariff_revision_periods
  FOR EACH ROW EXECUTE FUNCTION reject_rate_period_change();
-- Append-only record of the home's electric utility, read latest revision first.
CREATE TABLE home_electric_utility (
  home_id     uuid NOT NULL REFERENCES homes(id),
  revision    bigint NOT NULL CHECK (revision > 0),
  eiaid       text NOT NULL CHECK (eiaid ~ '^[0-9]+$'),
  chosen_by   text NOT NULL CHECK (chosen_by IN ('zip_single','user')),
  recorded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (home_id, revision)
);
CREATE FUNCTION reject_home_electric_utility_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'home electric utility rows are append-only'; END $$;
CREATE TRIGGER home_electric_utility_append_only BEFORE UPDATE OR DELETE ON home_electric_utility
  FOR EACH ROW EXECUTE FUNCTION reject_home_electric_utility_change();
-- datawiz load-rate-plans records its run report on the rate_plans route.
ALTER TABLE datawiz_runs DROP CONSTRAINT datawiz_runs_route_check,
  ADD CONSTRAINT datawiz_runs_route_check
  CHECK (route IN ('device','zip','savings','efficiency','resolve','catalog','rate_plans'));
