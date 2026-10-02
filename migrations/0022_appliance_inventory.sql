-- Owner-scoped inventory with immutable identity/evidence snapshots.
CREATE TABLE appliances (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(trim(name)) > 0),
  category text NOT NULL CHECK (length(trim(category)) > 0),
  room text,
  catalog_product_id uuid REFERENCES catalog_products(id),
  identity jsonb NOT NULL,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz,
  UNIQUE(home_id,id),
  CHECK (catalog_product_id IS NULL OR identity->>'match_quality' = 'confirmed_exact')
);
CREATE INDEX appliances_home_active_idx ON appliances(home_id) WHERE archived_at IS NULL;
CREATE TABLE appliance_revisions (
  appliance_id uuid NOT NULL,
  home_id uuid NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  snapshot jsonb NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(appliance_id,version),
  FOREIGN KEY (home_id,appliance_id) REFERENCES appliances(home_id,id) ON DELETE CASCADE
);
CREATE FUNCTION reject_appliance_revision_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'appliance revisions are immutable'; END $$;
CREATE TRIGGER appliance_revisions_immutable BEFORE UPDATE ON appliance_revisions FOR EACH ROW EXECUTE FUNCTION reject_appliance_revision_change();
CREATE TABLE appliance_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  operation text NOT NULL,
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(owner_user_id,home_id,operation,key)
);
