-- Cloud-issued sensor IDs are scoped to one hub ownership epoch. Old rows
-- remain for history but are never returned as current mappings after transfer.
CREATE TABLE sensor_endpoints (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  hub_id uuid NOT NULL REFERENCES hubs(id),
  ownership_epoch bigint NOT NULL CHECK (ownership_epoch > 0),
  local_sensor_id text NOT NULL CHECK (length(local_sensor_id) BETWEEN 1 AND 256),
  parent_sensor_id uuid,
  kind text NOT NULL CHECK (kind IN ('plug','strip_outlet','circuit','utility_meter')),
  capabilities text[] NOT NULL DEFAULT '{}' CHECK (capabilities <@ ARRAY['power','energy','voltage','current','relay_state']::text[]),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz,
  UNIQUE (home_id,id),
  UNIQUE (hub_id,ownership_epoch,local_sensor_id),
  FOREIGN KEY (home_id,parent_sensor_id) REFERENCES sensor_endpoints(home_id,id)
);
CREATE INDEX sensor_endpoints_current_idx ON sensor_endpoints(hub_id,ownership_epoch) WHERE archived_at IS NULL;
CREATE TABLE sensor_request_idempotency (
  owner_user_id uuid NOT NULL REFERENCES users(id),
  home_id uuid NOT NULL REFERENCES homes(id),
  key uuid NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  sensor_id uuid NOT NULL REFERENCES sensor_endpoints(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(owner_user_id,home_id,key)
);
