-- datawiz load-catalog records its run report as a datawiz_runs row on the catalog route.
ALTER TABLE datawiz_runs DROP CONSTRAINT datawiz_runs_route_check,
  ADD CONSTRAINT datawiz_runs_route_check
  CHECK (route IN ('device','zip','savings','efficiency','resolve','catalog'));
