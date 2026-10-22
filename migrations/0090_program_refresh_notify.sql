-- Wakes the Datawiz programs listener as soon as a refresh job row commits.
CREATE FUNCTION notify_program_discovery_refresh() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('program_discovery_refresh', NEW.id::text);
  RETURN NULL;
END;
$$;
CREATE TRIGGER program_discovery_refresh_jobs_notify
  AFTER INSERT ON program_discovery_refresh_jobs
  FOR EACH ROW EXECUTE FUNCTION notify_program_discovery_refresh();
