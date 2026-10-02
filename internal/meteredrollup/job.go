package meteredrollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxDirtyPerScan = 20000

var ErrTooManyBuckets = errors.New("ingest batch spans too many rollup buckets")

type Job struct{ Pool *pgxpool.Pool }
type Stats struct {
	BatchesScanned    int
	BucketsRecomputed int
	DetailRowsPurged  int
	SummariesPurged   int
}
type batchRef struct {
	Cursor int64
	Hub    string
	Epoch  int64
	ID     string
}
type dirtyRow struct {
	ID                      int64
	Kind, Home, Hub, Sensor string
	Epoch                   int64
	Attachment              *string
	Start                   time.Time
}

func floorUTC(t time.Time, step time.Duration) time.Time { return t.UTC().Truncate(step) }
func dayUTC(t time.Time) time.Time {
	v := t.UTC()
	return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
}
func nextDay(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
func (j Job) RunOnce(ctx context.Context, now time.Time, batchLimit, bucketLimit, purgeLimit int) (Stats, error) {
	if j.Pool == nil || batchLimit < 1 || batchLimit > 20 || bucketLimit < 1 || bucketLimit > 200 || purgeLimit < 0 || purgeLimit > 500 {
		return Stats{}, errors.New("invalid rollup job limits")
	}
	var out Stats
	var err error
	out.BatchesScanned, err = j.scan(ctx, batchLimit)
	if err != nil {
		return out, err
	}
	out.BucketsRecomputed, err = j.process(ctx, bucketLimit)
	if err != nil {
		return out, err
	}
	if purgeLimit > 0 {
		out.DetailRowsPurged, out.SummariesPurged, err = j.retain(ctx, now, purgeLimit)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
func (j Job) scan(ctx context.Context, limit int) (int, error) {
	tx, err := j.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// A short table lock waits for in-flight batch inserts, so identity cursors
	// cannot commit out of order across the durable checkpoint.
	if _, err = tx.Exec(ctx, `LOCK TABLE ingest_batches IN SHARE MODE`); err != nil {
		return 0, err
	}
	var last int64
	err = tx.QueryRow(ctx, `SELECT last_cursor FROM metered_rollup_checkpoint WHERE singleton=true FOR UPDATE`).Scan(&last)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT cursor,hub_id,ownership_epoch,batch_id FROM ingest_batches WHERE cursor>$1 ORDER BY cursor LIMIT $2`, last, limit)
	if err != nil {
		return 0, err
	}
	batches := []batchRef{}
	for rows.Next() {
		var b batchRef
		if err = rows.Scan(&b.Cursor, &b.Hub, &b.Epoch, &b.ID); err != nil {
			rows.Close()
			return 0, err
		}
		batches = append(batches, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	dirtyCount := 0
	for _, b := range batches {
		erows, e := tx.Query(ctx, `SELECT event_id,event_kind,home_id,sensor_id FROM ingest_events WHERE hub_id=$1 AND ownership_epoch=$2 AND first_batch_id=$3`, b.Hub, b.Epoch, b.ID)
		if e != nil {
			return 0, e
		}
		type event struct{ id, kind, home, sensor string }
		events := []event{}
		for erows.Next() {
			var v event
			if e = erows.Scan(&v.id, &v.kind, &v.home, &v.sensor); e != nil {
				erows.Close()
				return 0, e
			}
			events = append(events, v)
		}
		e = erows.Err()
		erows.Close()
		if e != nil {
			return 0, e
		}
		for _, v := range events {
			switch v.kind {
			case "power_point":
				drows, e := tx.Query(ctx, `SELECT observed_at FROM energy_observations WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND kind='power_point'`, b.Hub, b.Epoch, v.id)
				if e != nil {
					return 0, e
				}
				all := []time.Time{}
				for drows.Next() {
					var at time.Time
					if e = drows.Scan(&at); e != nil {
						drows.Close()
						return 0, e
					}
					all = append(all, at)
				}
				e = drows.Err()
				drows.Close()
				if e != nil {
					return 0, e
				}
				for _, at := range all {
					n, e := markRange(ctx, tx, "power", v.home, b.Hub, b.Epoch, v.sensor, nil, at, at.Add(time.Nanosecond), false)
					if e != nil {
						return 0, e
					}
					dirtyCount += n
				}
			case "energy_interval":
				drows, e := tx.Query(ctx, `SELECT start_at,end_at,attachment_revision_id FROM energy_observations WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND kind='energy_interval'`, b.Hub, b.Epoch, v.id)
				if e != nil {
					return 0, e
				}
				type interval struct {
					start, end time.Time
					attachment string
				}
				all := []interval{}
				for drows.Next() {
					var x interval
					if e = drows.Scan(&x.start, &x.end, &x.attachment); e != nil {
						drows.Close()
						return 0, e
					}
					all = append(all, x)
				}
				e = drows.Err()
				drows.Close()
				if e != nil {
					return 0, e
				}
				for _, x := range all {
					n, e := markRange(ctx, tx, "energy", v.home, b.Hub, b.Epoch, v.sensor, &x.attachment, x.start, x.end, false)
					if e != nil {
						return 0, e
					}
					dirtyCount += n
				}
			case "coverage_gap":
				drows, e := tx.Query(ctx, `SELECT first_at,last_at FROM coverage_gaps WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3`, b.Hub, b.Epoch, v.id)
				if e != nil {
					return 0, e
				}
				type gap struct{ start, end time.Time }
				all := []gap{}
				for drows.Next() {
					var x gap
					if e = drows.Scan(&x.start, &x.end); e != nil {
						drows.Close()
						return 0, e
					}
					all = append(all, x)
				}
				e = drows.Err()
				drows.Close()
				if e != nil {
					return 0, e
				}
				for _, x := range all {
					n, e := markRange(ctx, tx, "gap", v.home, b.Hub, b.Epoch, v.sensor, nil, x.start, x.end, true)
					if e != nil {
						return 0, e
					}
					dirtyCount += n
				}
			}
			if dirtyCount > maxDirtyPerScan {
				return 0, ErrTooManyBuckets
			}
		}
		last = b.Cursor
	}
	if len(batches) > 0 {
		_, err = tx.Exec(ctx, `UPDATE metered_rollup_checkpoint SET last_cursor=$1,updated_at=now() WHERE singleton=true`, last)
		if err != nil {
			return 0, err
		}
	}
	return len(batches), tx.Commit(ctx)
}
func markRange(ctx context.Context, tx pgx.Tx, prefix, home, hub string, epoch int64, sensor string, attachment *string, start, end time.Time, inclusive bool) (int, error) {
	if end.Before(start) || (!inclusive && !end.After(start)) {
		return 0, errors.New("invalid rollup source interval")
	}
	last := end
	if !inclusive {
		last = end.Add(-time.Nanosecond)
	}
	n := 0
	for bucket := floorUTC(start, time.Hour); !bucket.After(last); bucket = bucket.Add(time.Hour) {
		if err := mark(ctx, tx, prefix+"_hour", home, hub, epoch, sensor, attachment, bucket); err != nil {
			return n, err
		}
		n++
		if n > maxDirtyPerScan {
			return n, ErrTooManyBuckets
		}
	}
	for bucket := dayUTC(start); !bucket.After(last); bucket = nextDay(bucket) {
		if err := mark(ctx, tx, prefix+"_day", home, hub, epoch, sensor, attachment, bucket); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
func mark(ctx context.Context, tx pgx.Tx, kind, home, hub string, epoch int64, sensor string, attachment *string, bucket time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO metered_rollup_dirty(kind,home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT ON CONSTRAINT metered_rollup_dirty_scope DO UPDATE SET changed_at=now()`, kind, home, hub, epoch, sensor, attachment, bucket)
	return err
}
func (j Job) process(ctx context.Context, limit int) (int, error) {
	tx, err := j.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,kind,home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start FROM metered_rollup_dirty ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	dirty := []dirtyRow{}
	for rows.Next() {
		var x dirtyRow
		if err = rows.Scan(&x.ID, &x.Kind, &x.Home, &x.Hub, &x.Epoch, &x.Sensor, &x.Attachment, &x.Start); err != nil {
			rows.Close()
			return 0, err
		}
		dirty = append(dirty, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, x := range dirty {
		switch x.Kind {
		case "energy_hour", "energy_day":
			err = recomputeEnergy(ctx, tx, x)
		case "gap_hour", "gap_day":
			err = recomputeGap(ctx, tx, x)
		case "power_hour", "power_day":
			err = recomputePower(ctx, tx, x)
		default:
			err = fmt.Errorf("unknown dirty kind %q", x.Kind)
		}
		if err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM metered_rollup_dirty WHERE id=$1`, x.ID); err != nil {
			return 0, err
		}
	}
	return len(dirty), tx.Commit(ctx)
}
