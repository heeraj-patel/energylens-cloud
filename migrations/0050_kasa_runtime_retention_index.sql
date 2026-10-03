-- Supports ordered, bounded retention passes across all homes.
CREATE INDEX kasa_runtime_retention_idx ON kasa_runtime_snapshots(observed_at,hub_id,ownership_epoch,snapshot_id);
