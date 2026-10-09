package reports

// The same dependency rule is used by read, queue, and final delivery checks.
// Inputs are the preview UUID, its home UUID, and saved home version. The home row is locked by
// create, queue, and worker transactions when mutation serialization matters.
const dependencyStaleSQL = `SELECT h.version<>$3 OR EXISTS (SELECT 1 FROM report_preview_invalidations v WHERE v.preview_id=$1) OR EXISTS (
 SELECT 1 FROM report_preview_items i
 LEFT JOIN appliances a ON a.id=i.appliance_id AND a.home_id=h.id AND i.demo_device_id IS NULL
 WHERE i.preview_id=$1 AND (
  (i.demo_device_id IS NULL AND (a.id IS NULL OR a.archived_at IS NOT NULL OR a.version<>i.appliance_version OR
   (i.estimate_run_id IS NOT NULL AND EXISTS(SELECT 1 FROM estimate_run_invalidations v WHERE v.run_id=i.estimate_run_id))))
  OR (i.demo_device_id IS NOT NULL AND (h.demo_cost_snapshot IS NULL OR
   public.digest(h.demo_cost_snapshot::text,'sha256')<>i.demo_snapshot_sha256))
 )
) FROM homes h WHERE h.id=$2 AND h.deleted_at IS NULL`
