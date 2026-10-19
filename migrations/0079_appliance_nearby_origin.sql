ALTER TABLE appliances
  ADD COLUMN nearby_hub_id uuid,
  ADD COLUMN nearby_ownership_epoch bigint,
  ADD COLUMN nearby_protocol_device_id text;

ALTER TABLE appliances ADD CONSTRAINT appliance_nearby_origin_complete CHECK (
  (nearby_hub_id IS NULL AND nearby_ownership_epoch IS NULL AND nearby_protocol_device_id IS NULL)
  OR (nearby_hub_id IS NOT NULL AND nearby_ownership_epoch IS NOT NULL
      AND nearby_ownership_epoch > 0
      AND nearby_protocol_device_id IS NOT NULL
      AND length(nearby_protocol_device_id) BETWEEN 1 AND 256)
);

CREATE UNIQUE INDEX appliance_nearby_origin_active_unique
  ON appliances(home_id, nearby_hub_id, nearby_ownership_epoch, nearby_protocol_device_id)
  WHERE archived_at IS NULL AND nearby_hub_id IS NOT NULL;
