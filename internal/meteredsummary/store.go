package meteredsummary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home, sensor, or attachment not found")
var ErrRevisionChanged = errors.New("metered summary evidence changed")

type Store struct{ Pool *pgxpool.Pool }
type Query struct {
	HomeID, OwnerID, Granularity, SensorID, AttachmentID string
	From, To                                             time.Time
	Limit                                                int
	After                                                *Key
}
type Key struct {
	Bucket                        time.Time `json:"bucket"`
	Kind, Hub, Sensor, Attachment string
	Epoch                         int64
}
type Row struct {
	Payload json.RawMessage
	Key     Key
}

func (s Store) Scope(ctx context.Context, q Query) error {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, q.HomeID, q.OwnerID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if q.SensorID != "" {
		err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sensor_endpoints WHERE id=$1 AND home_id=$2)`, q.SensorID, q.HomeID).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
	}
	if q.AttachmentID != "" {
		err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE id=$1 AND home_id=$2 AND ($3='' OR sensor_id=NULLIF($3,'')::uuid))`, q.AttachmentID, q.HomeID, q.SensorID).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
	}
	return nil
}

// Each branch retains its own meaning. Power statistics and gap counts never
// appear as energy, and only explicit attachment revisions have energy rows.
const pageSQL = `WITH summaries AS (
 SELECT e.bucket_start,'energy'::text kind,e.hub_id,e.ownership_epoch,e.sensor_id,e.attachment_id,
 jsonb_build_object('kind','energy','bucket_start',e.bucket_start,'hub_id',e.hub_id,'ownership_epoch',e.ownership_epoch,'sensor_id',e.sensor_id,'attachment_id',e.attachment_id,'energy_wh',e.energy_wh,'covered_seconds',e.covered_seconds,'interval_count',e.interval_count,'status',e.status,'allocation_method',e.allocation_method,'source_refs',e.source_refs,'computed_at',e.computed_at) payload
 FROM %s e JOIN homes h ON h.id=e.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
 JOIN sensor_endpoints s ON s.id=e.sensor_id AND s.home_id=e.home_id AND s.hub_id=e.hub_id AND s.ownership_epoch=e.ownership_epoch
 JOIN attachments a ON a.id=e.attachment_id AND a.home_id=e.home_id AND a.sensor_id=e.sensor_id
 WHERE e.home_id=$1 AND e.bucket_start >= $3 AND e.bucket_start < $4 AND ($5='' OR e.sensor_id=NULLIF($5,'')::uuid) AND ($6='' OR e.attachment_id=NULLIF($6,'')::uuid)
 UNION ALL
 SELECT p.bucket_start,'power_sample_stats'::text,p.hub_id,p.ownership_epoch,p.sensor_id,NULL::uuid,
 jsonb_build_object('kind','power_sample_stats','bucket_start',p.bucket_start,'hub_id',p.hub_id,'ownership_epoch',p.ownership_epoch,'sensor_id',p.sensor_id,'sample_count',p.sample_count,'min_watts',p.min_watts,'max_watts',p.max_watts,'mean_watts',p.mean_watts,'source_refs',p.source_refs,'computed_at',p.computed_at)
 FROM %s p JOIN homes h ON h.id=p.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
 JOIN sensor_endpoints s ON s.id=p.sensor_id AND s.home_id=p.home_id AND s.hub_id=p.hub_id AND s.ownership_epoch=p.ownership_epoch
 WHERE p.home_id=$1 AND p.bucket_start >= $3 AND p.bucket_start < $4 AND ($5='' OR p.sensor_id=NULLIF($5,'')::uuid) AND $6=''
 UNION ALL
 SELECT c.bucket_start,'coverage_gap'::text,c.hub_id,c.ownership_epoch,c.sensor_id,NULL::uuid,
 jsonb_build_object('kind','coverage_gap','bucket_start',c.bucket_start,'hub_id',c.hub_id,'ownership_epoch',c.ownership_epoch,'sensor_id',c.sensor_id,'gap_overlap_count',c.gap_overlap_count,'dropped_count_started',c.dropped_count_started,'source_refs',c.source_refs,'computed_at',c.computed_at)
 FROM %s c JOIN homes h ON h.id=c.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
 JOIN sensor_endpoints s ON s.id=c.sensor_id AND s.home_id=c.home_id AND s.hub_id=c.hub_id AND s.ownership_epoch=c.ownership_epoch
 WHERE c.home_id=$1 AND c.bucket_start >= $3 AND c.bucket_start < $4 AND ($5='' OR c.sensor_id=NULLIF($5,'')::uuid) AND $6=''
) SELECT payload,bucket_start,kind,hub_id,ownership_epoch,sensor_id,attachment_id FROM summaries
WHERE ($7::timestamptz IS NULL OR (bucket_start,kind,hub_id::text,ownership_epoch,sensor_id::text,COALESCE(attachment_id::text,'')) < ($7,$8,$9,$10,$11,$12))
ORDER BY bucket_start DESC,kind DESC,hub_id DESC,ownership_epoch DESC,sensor_id DESC,attachment_id DESC NULLS LAST LIMIT $13`

func (s Store) Page(ctx context.Context, q Query) ([]Row, error) {
	return page(ctx, s.Pool, q)
}

// PageAtRevision reads the revision and page from one database snapshot. A
// correction committed between requests causes a conflict before any later
// page is returned, including when an earlier row was corrected or removed.
func (s Store) PageAtRevision(ctx context.Context, q Query, expected *int64) ([]Row, int64, error) {
	rows, revision, _, err := s.PageAtRevisionWithHistory(ctx, q, expected, false)
	return rows, revision, err
}

// PageAtRevisionWithHistory checks home-wide evidence in the same snapshot as
// the page, so a concurrent insert cannot turn an empty real page into sample.
func (s Store) PageAtRevisionWithHistory(ctx context.Context, q Query, expected *int64, checkHistory bool) ([]Row, int64, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, false, err
	}
	defer tx.Rollback(ctx)
	var revision int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(r.revision,0) FROM homes h LEFT JOIN metered_summary_revision r ON r.home_id=h.id WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, q.HomeID, q.OwnerID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, ErrNotFound
	}
	if err != nil {
		return nil, 0, false, err
	}
	if expected != nil && *expected != revision {
		return nil, 0, false, ErrRevisionChanged
	}
	rows, err := page(ctx, tx, q)
	if err != nil {
		return nil, 0, false, err
	}
	var hasHistory bool
	if checkHistory && len(rows) == 0 {
		err = tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM energy_hourly WHERE home_id=$1 UNION ALL
			SELECT 1 FROM energy_daily WHERE home_id=$1 UNION ALL
			SELECT 1 FROM power_hourly WHERE home_id=$1 UNION ALL
			SELECT 1 FROM power_daily WHERE home_id=$1 UNION ALL
			SELECT 1 FROM coverage_hourly WHERE home_id=$1 UNION ALL
			SELECT 1 FROM coverage_daily WHERE home_id=$1
		)`, q.HomeID).Scan(&hasHistory)
		if err != nil {
			return nil, 0, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, false, err
	}
	return rows, revision, hasHistory, nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func page(ctx context.Context, db queryer, q Query) ([]Row, error) {
	tables := map[string][3]string{"hour": {"energy_hourly", "power_hourly", "coverage_hourly"}, "day": {"energy_daily", "power_daily", "coverage_daily"}}
	names, ok := tables[q.Granularity]
	if !ok {
		return nil, errors.New("invalid granularity")
	}
	var after any
	var key Key
	if q.After != nil {
		after = q.After.Bucket
		key = *q.After
	}
	sql := fmt.Sprintf(pageSQL, names[0], names[1], names[2])
	rows, err := db.Query(ctx, sql, q.HomeID, q.OwnerID, q.From, q.To, q.SensorID, q.AttachmentID, after, key.Kind, key.Hub, key.Epoch, key.Sensor, key.Attachment, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var row Row
		var attachment *string
		if err = rows.Scan(&row.Payload, &row.Key.Bucket, &row.Key.Kind, &row.Key.Hub, &row.Key.Epoch, &row.Key.Sensor, &attachment); err != nil {
			return nil, err
		}
		if attachment != nil {
			row.Key.Attachment = *attachment
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
