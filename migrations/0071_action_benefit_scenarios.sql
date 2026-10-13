-- A reviewed, action-specific alternative to one active usage fact.
CREATE TABLE reviewed_action_benefit_publications (
  payload_sha256 text PRIMARY KEY CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
  key_id text NOT NULL CHECK (key_id ~ '^[0-9a-f]{64}$'),
  envelope_sha256 text NOT NULL CHECK (envelope_sha256 ~ '^[0-9a-f]{64}$'),
  imported_at timestamptz NOT NULL DEFAULT now(),
  imported_by text NOT NULL CHECK (length(trim(imported_by)) > 0),
  scenario_count integer NOT NULL CHECK (scenario_count >= 0)
);
CREATE TABLE reviewed_action_benefit_scenarios (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  publication_sha256 text NOT NULL REFERENCES reviewed_action_benefit_publications(payload_sha256),
  rule_id text NOT NULL CHECK (length(trim(rule_id)) > 0),
  category text NOT NULL,
  manufacturer text NOT NULL,
  model text NOT NULL,
  variant text NOT NULL,
  fuel text NOT NULL CHECK (fuel = 'electricity'),
  baseline_fact_id uuid NOT NULL REFERENCES reviewed_usage_facts(id),
  alternative_kwh numeric NOT NULL CHECK (alternative_kwh >= 0),
  usage_unit text NOT NULL CHECK (usage_unit IN ('kWh/use','kWh/hour')),
  service_definition text NOT NULL CHECK (length(trim(service_definition)) > 0),
  exclusion_group text NOT NULL CHECK (length(trim(exclusion_group)) > 0),
  source_url text NOT NULL CHECK (length(trim(source_url)) > 0),
  source_sha256 text NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  UNIQUE (rule_id,category,manufacturer,model,variant,revision)
);
CREATE TABLE reviewed_action_benefit_scenario_changes (
  prior_id uuid PRIMARY KEY REFERENCES reviewed_action_benefit_scenarios(id),
  replacement_id uuid UNIQUE REFERENCES reviewed_action_benefit_scenarios(id),
  publication_sha256 text NOT NULL REFERENCES reviewed_action_benefit_publications(payload_sha256),
  action text NOT NULL CHECK (action IN ('supersede','retract')),
  reason text NOT NULL CHECK (length(trim(reason)) > 0),
  reviewer text NOT NULL CHECK (length(trim(reviewer)) > 0),
  reviewed_at timestamptz NOT NULL,
  review_sha256 text NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
  CHECK ((action='supersede' AND replacement_id IS NOT NULL AND replacement_id<>prior_id)
      OR (action='retract' AND replacement_id IS NULL))
);
CREATE VIEW active_reviewed_action_benefit_scenarios AS
SELECT s.* FROM reviewed_action_benefit_scenarios s
WHERE NOT EXISTS (SELECT 1 FROM reviewed_action_benefit_scenario_changes c WHERE c.prior_id=s.id);
CREATE TRIGGER reviewed_action_benefit_scenarios_immutable BEFORE UPDATE OR DELETE ON reviewed_action_benefit_scenarios
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER reviewed_action_benefit_publications_immutable BEFORE UPDATE OR DELETE ON reviewed_action_benefit_publications
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
CREATE TRIGGER reviewed_action_benefit_scenario_changes_immutable BEFORE UPDATE OR DELETE ON reviewed_action_benefit_scenario_changes
  FOR EACH ROW EXECUTE FUNCTION reject_catalog_import_change();
