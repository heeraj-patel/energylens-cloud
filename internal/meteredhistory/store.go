package meteredhistory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home, sensor, or attachment not found")

type Store struct{ Pool *pgxpool.Pool }
type Query struct {
	HomeID         string
	OwnerID        string
	From           time.Time
	To             time.Time
	SensorID       string
	AttachmentID   string
	Limit          int
	SnapshotCursor int64
	AfterTime      *time.Time
	AfterHub       string
	AfterEpoch     int64
	AfterEvent     string
}
type Row struct {
	Payload  json.RawMessage
	SortTime time.Time
	HubID    string
	Epoch    int64
	EventID  string
}

func (s Store) Scope(ctx context.Context, q Query) error {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, q.HomeID, q.OwnerID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if q.SensorID != "" {
		err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sensor_endpoints WHERE id=$1 AND home_id=$2)`, q.SensorID, q.HomeID).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	if q.AttachmentID != "" {
		err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE id=$1 AND home_id=$2)`, q.AttachmentID, q.HomeID).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// Snapshot waits for in-flight inserts, then freezes the highest committed
// ingest cursor. Later correction revisions cannot change subsequent pages.
func (s Store) Snapshot(ctx context.Context) (int64, time.Time, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE ingest_batches IN SHARE MODE`); err != nil {
		return 0, time.Time{}, err
	}
	var cursor int64
	var asOf time.Time
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(cursor),0),clock_timestamp() FROM ingest_batches`).Scan(&cursor, &asOf)
	if err != nil {
		return 0, time.Time{}, err
	}
	return cursor, asOf, tx.Commit(ctx)
}

const historySQL = `WITH visible AS (
 SELECT e.hub_id,e.ownership_epoch,e.event_id,e.revision,e.home_id,e.sensor_id,e.event_kind,e.accepted_at,
        row_number() OVER(PARTITION BY e.hub_id,e.ownership_epoch,e.event_id ORDER BY e.revision DESC) AS rn
 FROM ingest_events e
 JOIN ingest_batches b ON b.hub_id=e.hub_id AND b.ownership_epoch=e.ownership_epoch AND b.batch_id=e.first_batch_id
 JOIN homes h ON h.id=e.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
 WHERE e.home_id=$1 AND b.cursor<=$3
), latest AS (SELECT * FROM visible WHERE rn=1), history AS (
 SELECT l.sensor_id,NULL::uuid AS attachment_id,o.observed_at AS sort_time,l.hub_id,l.ownership_epoch,l.event_id,
 jsonb_strip_nulls(jsonb_build_object('kind','power_point','event_id',l.event_id,'revision',l.revision,'sensor_id',l.sensor_id,'hub_id',l.hub_id,'ownership_epoch',l.ownership_epoch,'accepted_at',l.accepted_at,'timestamp',o.observed_at,'watts',o.watts,'kwh_cumulative',o.kwh_cumulative,'source_method',o.source_method,'source_transport',o.source_transport)) AS payload
 FROM latest l JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision WHERE l.event_kind='power_point'
 UNION ALL
 SELECT l.sensor_id,o.attachment_revision_id,o.start_at,l.hub_id,l.ownership_epoch,l.event_id,
 jsonb_strip_nulls(jsonb_build_object('kind','energy_interval','event_id',l.event_id,'revision',l.revision,'sensor_id',l.sensor_id,'hub_id',l.hub_id,'ownership_epoch',l.ownership_epoch,'accepted_at',l.accepted_at,'start',o.start_at,'end',o.end_at,'energy_wh',o.energy_wh,'covered_seconds',o.covered_seconds,'attachment_id',o.attachment_revision_id,'appliance_id',a.appliance_id,'fuel',o.fuel,'quality',o.quality,'source',o.source,'source_method',o.source_method))
 FROM latest l JOIN energy_observations o ON o.hub_id=l.hub_id AND o.ownership_epoch=l.ownership_epoch AND o.event_id=l.event_id AND o.revision=l.revision JOIN attachments a ON a.id=o.attachment_revision_id AND a.home_id=l.home_id WHERE l.event_kind='energy_interval'
 UNION ALL
 SELECT l.sensor_id,NULL::uuid,g.first_at,l.hub_id,l.ownership_epoch,l.event_id,
 jsonb_build_object('kind','coverage_gap','event_id',l.event_id,'revision',l.revision,'sensor_id',l.sensor_id,'hub_id',l.hub_id,'ownership_epoch',l.ownership_epoch,'accepted_at',l.accepted_at,'first_timestamp',g.first_at,'last_timestamp',g.last_at,'reason',g.reason,'dropped_count',g.dropped_count)
 FROM latest l JOIN coverage_gaps g ON g.hub_id=l.hub_id AND g.ownership_epoch=l.ownership_epoch AND g.event_id=l.event_id AND g.revision=l.revision WHERE l.event_kind='coverage_gap'
)
SELECT payload,sort_time,hub_id,ownership_epoch,event_id FROM history
WHERE sort_time >= $4 AND sort_time < $5
 AND ($6='' OR sensor_id=NULLIF($6,'')::uuid)
 AND ($7='' OR attachment_id=NULLIF($7,'')::uuid)
 AND ($8::timestamptz IS NULL OR (sort_time,hub_id,ownership_epoch,event_id)<($8,NULLIF($9,'')::uuid,$10,NULLIF($11,'')::uuid))
ORDER BY sort_time DESC,hub_id DESC,ownership_epoch DESC,event_id DESC LIMIT $12`

func (s Store) Page(ctx context.Context, q Query) ([]Row, error) {
	var after any
	if q.AfterTime != nil {
		after = *q.AfterTime
	}
	rows, err := s.Pool.Query(ctx, historySQL, q.HomeID, q.OwnerID, q.SnapshotCursor, q.From, q.To, q.SensorID, q.AttachmentID, after, q.AfterHub, q.AfterEpoch, q.AfterEvent, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var v Row
		err = rows.Scan(&v.Payload, &v.SortTime, &v.HubID, &v.Epoch, &v.EventID)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
