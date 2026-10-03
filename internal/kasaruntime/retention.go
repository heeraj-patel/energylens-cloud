package kasaruntime

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionPeriod exceeds both the 30-day read window and the 31-day
// continuation cursor allowance. Rows at the boundary remain available.
const RetentionPeriod = 31 * 24 * time.Hour

// RetentionJob deletes expired snapshots in bounded passes. Row locks let
// concurrent workers skip busy rows and retry them on a later pass.
type RetentionJob struct{ DB *pgxpool.Pool }

func (j RetentionJob) RunOnce(ctx context.Context, now time.Time, limit int) (int64, error) {
	if j.DB == nil || limit < 1 || limit > 500 {
		return 0, errors.New("Kasa runtime retention requires a database and a limit from 1 to 500")
	}
	tag, err := j.DB.Exec(ctx, `WITH expired AS (
		SELECT hub_id,ownership_epoch,snapshot_id FROM kasa_runtime_snapshots
		WHERE observed_at < $1
		ORDER BY observed_at,hub_id,ownership_epoch,snapshot_id
		LIMIT $2 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM kasa_runtime_snapshots k USING expired e
	WHERE k.hub_id=e.hub_id AND k.ownership_epoch=e.ownership_epoch AND k.snapshot_id=e.snapshot_id`, now.UTC().Add(-RetentionPeriod), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
