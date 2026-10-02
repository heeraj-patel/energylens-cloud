-- Bound recent owner history reads to one home before latest-revision selection.
CREATE INDEX ingest_events_home_latest_idx
  ON ingest_events(home_id,hub_id,ownership_epoch,event_id,revision DESC);
