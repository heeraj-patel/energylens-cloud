-- Definitions are versioned in the API registry. Answers bind to the exact
-- appliance revision so changed identity/category cannot reuse old usage.
CREATE TABLE question_answers (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id),
  appliance_id uuid NOT NULL,
  appliance_version bigint NOT NULL CHECK (appliance_version > 0),
  question_id text NOT NULL CHECK (length(trim(question_id)) > 0),
  question_version integer NOT NULL CHECK (question_version > 0),
  revision bigint NOT NULL CHECK (revision > 0),
  status text NOT NULL CHECK (status IN ('confirmed','unknown')),
  answer_value jsonb,
  confirmed_at timestamptz,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id),
  CHECK ((status='unknown' AND answer_value IS NULL AND confirmed_at IS NULL) OR
         (status='confirmed' AND answer_value IS NOT NULL AND confirmed_at IS NOT NULL)),
  UNIQUE (home_id,appliance_id,question_id,revision)
);
CREATE INDEX question_answers_current_idx ON question_answers(home_id,appliance_id,question_id,revision DESC);
CREATE TABLE question_answer_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_user_id,home_id,key)
);
CREATE FUNCTION reject_question_answer_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'question answer revisions are immutable'; END $$;
CREATE TRIGGER question_answers_immutable BEFORE UPDATE OR DELETE ON question_answers
  FOR EACH ROW EXECUTE FUNCTION reject_question_answer_change();
CREATE FUNCTION invalidate_answer_estimates() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO estimate_run_invalidations(run_id,reason)
  SELECT id,'dependency_changed' FROM estimate_runs WHERE appliance_id=NEW.appliance_id
  ON CONFLICT (run_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER question_answers_estimates_changed AFTER INSERT ON question_answers
  FOR EACH ROW EXECUTE FUNCTION invalidate_answer_estimates();
