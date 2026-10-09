ALTER TABLE report_preview_items ADD COLUMN demo_device_id text;
ALTER TABLE report_preview_items ADD COLUMN demo_snapshot_sha256 bytea;
ALTER TABLE report_preview_items ALTER COLUMN appliance_version DROP NOT NULL;
ALTER TABLE report_preview_items DROP CONSTRAINT report_preview_items_appliance_version_check;
ALTER TABLE report_preview_items ADD CONSTRAINT report_preview_items_dependency_shape CHECK (
  (demo_device_id IS NULL AND demo_snapshot_sha256 IS NULL
   AND appliance_version IS NOT NULL AND appliance_version > 0)
  OR
  (demo_device_id IS NOT NULL AND length(demo_device_id) > 0
   AND demo_snapshot_sha256 IS NOT NULL AND octet_length(demo_snapshot_sha256) = 32
   AND appliance_version IS NULL AND estimate_run_id IS NULL)
);
