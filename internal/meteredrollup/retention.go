package meteredrollup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type detailedObservation struct {
	Hub, Event, Kind, Home, Sensor string
	Epoch, Revision                int64
	Start, End                     *time.Time
	Attachment                     *string
	AcceptedAt                     time.Time
}
type detailedGap struct {
	Hub, Event, Home, Sensor string
	Epoch, Revision          int64
	First, Last              time.Time
	AcceptedAt               time.Time
}

func (j Job) retain(ctx context.Context, now time.Time, limit int) (int, int, error) {
	tx, err := j.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	// Keep the checkpoint and purge decision consistent with committed batches.
	if _, err = tx.Exec(ctx, `LOCK TABLE ingest_batches IN SHARE MODE`); err != nil {
		return 0, 0, err
	}
	var caughtUp bool
	err = tx.QueryRow(ctx, `SELECT c.last_cursor>=COALESCE((SELECT max(cursor) FROM ingest_batches),0) AND NOT EXISTS(SELECT 1 FROM metered_rollup_dirty) FROM metered_rollup_checkpoint c WHERE singleton=true FOR UPDATE OF c`).Scan(&caughtUp)
	if err != nil {
		return 0, 0, err
	}
	detailCount, summaryCount := 0, 0
	if caughtUp {
		cutoff := now.UTC().Add(-30 * 24 * time.Hour)
		rows, e := tx.Query(ctx, `SELECT o.hub_id,o.ownership_epoch,o.event_id,o.revision,o.kind,e.home_id,e.sensor_id,o.observed_at,o.start_at,o.end_at,o.attachment_revision_id,e.accepted_at
 FROM energy_observations o JOIN ingest_events e ON e.hub_id=o.hub_id AND e.ownership_epoch=o.ownership_epoch AND e.event_id=o.event_id AND e.revision=o.revision
 WHERE e.accepted_at<$1 AND ((o.kind='power_point' AND date_trunc('day',o.observed_at AT TIME ZONE 'UTC')+interval '1 day'<=$1 AT TIME ZONE 'UTC') OR (o.kind='energy_interval' AND date_trunc('day',o.end_at AT TIME ZONE 'UTC')+interval '1 day'<=$1 AT TIME ZONE 'UTC'))
 ORDER BY COALESCE(o.observed_at,o.end_at) LIMIT $2 FOR UPDATE OF o SKIP LOCKED`, cutoff, limit)
		if e != nil {
			return 0, 0, e
		}
		observations := []detailedObservation{}
		for rows.Next() {
			var v detailedObservation
			var observed, start *time.Time
			if e = rows.Scan(&v.Hub, &v.Epoch, &v.Event, &v.Revision, &v.Kind, &v.Home, &v.Sensor, &observed, &start, &v.End, &v.Attachment, &v.AcceptedAt); e != nil {
				rows.Close()
				return 0, 0, e
			}
			if v.Kind == "power_point" {
				v.Start = observed
			} else {
				v.Start = start
			}
			observations = append(observations, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return 0, 0, e
		}
		for _, v := range observations {
			ready := true
			if v.Kind == "energy_interval" {
				if v.Start == nil || v.End == nil || v.Attachment == nil {
					return 0, 0, fmt.Errorf("invalid interval retention row %s", v.Event)
				}
				ready, e = energySummaryReady(ctx, tx, v)
			} else {
				ready, e = powerSummaryReady(ctx, tx, v)
			}
			if e != nil {
				return 0, 0, e
			}
			if !ready {
				continue
			}
			tag, e := tx.Exec(ctx, `DELETE FROM energy_observations WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND revision=$4`, v.Hub, v.Epoch, v.Event, v.Revision)
			if e != nil {
				return 0, 0, e
			}
			detailCount += int(tag.RowsAffected())
		}
		grows, e := tx.Query(ctx, `SELECT g.hub_id,g.ownership_epoch,g.event_id,g.revision,e.home_id,e.sensor_id,g.first_at,g.last_at,e.accepted_at FROM coverage_gaps g JOIN ingest_events e ON e.hub_id=g.hub_id AND e.ownership_epoch=g.ownership_epoch AND e.event_id=g.event_id AND e.revision=g.revision WHERE date_trunc('day',g.last_at AT TIME ZONE 'UTC')+interval '1 day'<=$1 AT TIME ZONE 'UTC' AND e.accepted_at<$1 ORDER BY g.last_at LIMIT $2 FOR UPDATE OF g SKIP LOCKED`, cutoff, limit)
		if e != nil {
			return 0, 0, e
		}
		gaps := []detailedGap{}
		for grows.Next() {
			var v detailedGap
			if e = grows.Scan(&v.Hub, &v.Epoch, &v.Event, &v.Revision, &v.Home, &v.Sensor, &v.First, &v.Last, &v.AcceptedAt); e != nil {
				grows.Close()
				return 0, 0, e
			}
			gaps = append(gaps, v)
		}
		e = grows.Err()
		grows.Close()
		if e != nil {
			return 0, 0, e
		}
		for _, v := range gaps {
			ready, e := gapSummaryReady(ctx, tx, v)
			if e != nil {
				return 0, 0, e
			}
			if !ready {
				continue
			}
			tag, e := tx.Exec(ctx, `DELETE FROM coverage_gaps WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND revision=$4`, v.Hub, v.Epoch, v.Event, v.Revision)
			if e != nil {
				return 0, 0, e
			}
			detailCount += int(tag.RowsAffected())
		}
	}
	summaryCutoff := now.UTC().AddDate(-2, 0, 0)
	// Saved cycle snapshots have their own bounded lifetime and contain no
	// raw observations. Prune them in the same bounded retention pass.
	_, err = tx.Exec(ctx, `DELETE FROM dishwasher_cycle_evidence WHERE ctid IN (
 SELECT ctid FROM dishwasher_cycle_evidence WHERE expires_at<=$1 ORDER BY expires_at LIMIT $2)`, now.UTC(), limit)
	if err != nil {
		return 0, 0, err
	}
	for _, table := range []string{"energy_hourly", "energy_daily", "coverage_hourly", "coverage_daily", "power_hourly", "power_daily"} {
		sql := fmt.Sprintf(`DELETE FROM %s WHERE ctid IN (SELECT ctid FROM %s WHERE bucket_start<$1 ORDER BY bucket_start LIMIT $2)`, table, table)
		tag, e := tx.Exec(ctx, sql, summaryCutoff, limit)
		if e != nil {
			return 0, 0, e
		}
		summaryCount += int(tag.RowsAffected())
	}
	// Keep replay hashes and acknowledgements far beyond the 7-day hub retry
	// window. Prune only after their raw rows and 24-month summaries are gone.
	tombstoneCutoff := now.UTC().AddDate(-2, -1, 0)
	_, err = tx.Exec(ctx, `DELETE FROM ingest_events WHERE ctid IN (
 SELECT e.ctid FROM ingest_events e WHERE e.accepted_at<$1
 AND NOT EXISTS(SELECT 1 FROM energy_observations o WHERE o.hub_id=e.hub_id AND o.ownership_epoch=e.ownership_epoch AND o.event_id=e.event_id AND o.revision=e.revision)
 AND NOT EXISTS(SELECT 1 FROM coverage_gaps g WHERE g.hub_id=e.hub_id AND g.ownership_epoch=e.ownership_epoch AND g.event_id=e.event_id AND g.revision=e.revision)
 LIMIT $2)`, tombstoneCutoff, limit)
	if err != nil {
		return 0, 0, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM ingest_batches WHERE ctid IN (
 SELECT b.ctid FROM ingest_batches b WHERE b.accepted_at<$1
 AND NOT EXISTS(SELECT 1 FROM ingest_events e WHERE e.hub_id=b.hub_id AND e.ownership_epoch=b.ownership_epoch AND e.first_batch_id=b.batch_id)
 LIMIT $2)`, tombstoneCutoff, limit)
	if err != nil {
		return 0, 0, err
	}
	return detailCount, summaryCount, tx.Commit(ctx)
}
func energySummaryReady(ctx context.Context, tx pgx.Tx, v detailedObservation) (bool, error) {
	latest, err := latestRevision(ctx, tx, v.Hub, v.Epoch, v.Event)
	if err != nil {
		return false, err
	}
	for _, kind := range []string{"hour", "day"} {
		var bucket time.Time
		var advance func(time.Time) time.Time
		var table string
		if kind == "hour" {
			bucket = floorUTC(*v.Start, time.Hour)
			advance = func(t time.Time) time.Time { return t.Add(time.Hour) }
			table = "energy_hourly"
		} else {
			bucket = dayUTC(*v.Start)
			advance = nextDay
			table = "energy_daily"
		}
		last := v.End.Add(-time.Nanosecond)
		for !bucket.After(last) {
			var exists bool
			sql := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE home_id=$1 AND hub_id=$2 AND ownership_epoch=$3 AND sensor_id=$4 AND attachment_id=$5 AND bucket_start=$6 AND status<>'overlap_conflict' AND ($7::boolean OR source_refs @> jsonb_build_array(jsonb_build_object('event_id',$8::text,'revision',$9::bigint))))`, table)
			err := tx.QueryRow(ctx, sql, v.Home, v.Hub, v.Epoch, v.Sensor, *v.Attachment, bucket, v.Revision != latest, v.Event, v.Revision).Scan(&exists)
			if err != nil {
				return false, err
			}
			if !exists {
				return false, nil
			}
			bucket = advance(bucket)
		}
	}
	return true, nil
}
func gapSummaryReady(ctx context.Context, tx pgx.Tx, v detailedGap) (bool, error) {
	latest, err := latestRevision(ctx, tx, v.Hub, v.Epoch, v.Event)
	if err != nil {
		return false, err
	}
	for _, kind := range []string{"hour", "day"} {
		var bucket time.Time
		var advance func(time.Time) time.Time
		var table string
		if kind == "hour" {
			bucket = floorUTC(v.First, time.Hour)
			advance = func(t time.Time) time.Time { return t.Add(time.Hour) }
			table = "coverage_hourly"
		} else {
			bucket = dayUTC(v.First)
			advance = nextDay
			table = "coverage_daily"
		}
		for !bucket.After(v.Last) {
			var exists bool
			sql := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE home_id=$1 AND hub_id=$2 AND ownership_epoch=$3 AND sensor_id=$4 AND bucket_start=$5 AND ($6::boolean OR source_refs @> jsonb_build_array(jsonb_build_object('event_id',$7::text,'revision',$8::bigint))))`, table)
			err := tx.QueryRow(ctx, sql, v.Home, v.Hub, v.Epoch, v.Sensor, bucket, v.Revision != latest, v.Event, v.Revision).Scan(&exists)
			if err != nil {
				return false, err
			}
			if !exists {
				return false, nil
			}
			bucket = advance(bucket)
		}
	}
	return true, nil
}
func powerSummaryReady(ctx context.Context, tx pgx.Tx, v detailedObservation) (bool, error) {
	if v.Start == nil {
		return false, fmt.Errorf("invalid power retention row %s", v.Event)
	}
	latest, err := latestRevision(ctx, tx, v.Hub, v.Epoch, v.Event)
	if err != nil {
		return false, err
	}
	for _, table := range []string{"power_hourly", "power_daily"} {
		bucket := floorUTC(*v.Start, time.Hour)
		if table == "power_daily" {
			bucket = dayUTC(*v.Start)
		}
		var exists bool
		sql := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE home_id=$1 AND hub_id=$2 AND ownership_epoch=$3 AND sensor_id=$4 AND bucket_start=$5 AND ($6::boolean OR source_refs @> jsonb_build_array(jsonb_build_object('event_id',$7::text,'revision',$8::bigint))))`, table)
		if err := tx.QueryRow(ctx, sql, v.Home, v.Hub, v.Epoch, v.Sensor, bucket, v.Revision != latest, v.Event, v.Revision).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}
	return true, nil
}
func latestRevision(ctx context.Context, tx pgx.Tx, hub string, epoch int64, event string) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT max(revision) FROM ingest_events WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3`, hub, epoch, event).Scan(&revision)
	return revision, err
}
