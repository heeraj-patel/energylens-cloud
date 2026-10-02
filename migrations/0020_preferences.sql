-- Current preferences are a projection of immutable, versioned revisions.
CREATE TABLE account_preferences (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  version bigint NOT NULL DEFAULT 1 CHECK (version>0),
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version=1),
  email_reports_enabled boolean NOT NULL DEFAULT false,
  history_disclosure_accepted_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE account_preference_revisions (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  version bigint NOT NULL CHECK (version>0),
  schema_version integer NOT NULL CHECK (schema_version=1),
  email_reports_enabled boolean NOT NULL,
  history_disclosure_accepted_at timestamptz,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(user_id,version)
);
CREATE TABLE home_preferences (
  home_id uuid PRIMARY KEY REFERENCES homes(id) ON DELETE CASCADE,
  version bigint NOT NULL DEFAULT 1 CHECK (version>0),
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version=1),
  goal text NOT NULL DEFAULT 'unspecified' CHECK (goal IN ('reduce_cost','reduce_energy','understand_usage','unspecified')),
  comfort_priority text NOT NULL DEFAULT 'unknown' CHECK (comfort_priority IN ('high','balanced','flexible','unknown')),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE home_preference_revisions (
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  version bigint NOT NULL CHECK (version>0),
  schema_version integer NOT NULL CHECK (schema_version=1),
  goal text NOT NULL CHECK (goal IN ('reduce_cost','reduce_energy','understand_usage','unspecified')),
  comfort_priority text NOT NULL CHECK (comfort_priority IN ('high','balanced','flexible','unknown')),
  recorded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(home_id,version)
);
CREATE TABLE preference_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  operation text NOT NULL,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(owner_user_id,operation,key)
);
CREATE FUNCTION init_account_preferences() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO account_preferences(user_id) VALUES(NEW.id);
  INSERT INTO account_preference_revisions(user_id,version,schema_version,email_reports_enabled,history_disclosure_accepted_at)
    VALUES(NEW.id,1,1,false,NULL);
  RETURN NEW;
END $$;
CREATE TRIGGER users_init_preferences AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION init_account_preferences();
CREATE FUNCTION init_home_preferences() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO home_preferences(home_id) VALUES(NEW.id);
  INSERT INTO home_preference_revisions(home_id,version,schema_version,goal,comfort_priority)
    VALUES(NEW.id,1,1,'unspecified','unknown');
  RETURN NEW;
END $$;
CREATE TRIGGER homes_init_preferences AFTER INSERT ON homes FOR EACH ROW EXECUTE FUNCTION init_home_preferences();
INSERT INTO account_preferences(user_id) SELECT id FROM users ON CONFLICT DO NOTHING;
INSERT INTO account_preference_revisions(user_id,version,schema_version,email_reports_enabled,history_disclosure_accepted_at)
  SELECT user_id,version,schema_version,email_reports_enabled,history_disclosure_accepted_at FROM account_preferences ON CONFLICT DO NOTHING;
INSERT INTO home_preferences(home_id) SELECT id FROM homes ON CONFLICT DO NOTHING;
INSERT INTO home_preference_revisions(home_id,version,schema_version,goal,comfort_priority)
  SELECT home_id,version,schema_version,goal,comfort_priority FROM home_preferences ON CONFLICT DO NOTHING;
CREATE FUNCTION reject_preference_revision_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'preference revisions are immutable'; END $$;
CREATE TRIGGER account_revisions_immutable BEFORE UPDATE ON account_preference_revisions FOR EACH ROW EXECUTE FUNCTION reject_preference_revision_change();
CREATE TRIGGER home_revisions_immutable BEFORE UPDATE ON home_preference_revisions FOR EACH ROW EXECUTE FUNCTION reject_preference_revision_change();
