package reports

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const RetentionPeriod = 30 * 24 * time.Hour

// RetentionJob removes expired private snapshots and their dependent delivery
// requests. Its bounded pass can run alongside delivery; locked previews are
// skipped and retried on a later pass.
type RetentionJob struct{ DB *pgxpool.Pool }

func (j RetentionJob) RunOnce(ctx context.Context, now time.Time, limit int) (int64, error) {
	if j.DB == nil || limit < 1 || limit > 500 {
		return 0, errors.New("report retention requires a database and a limit from 1 to 500")
	}
	tag, err := j.DB.Exec(ctx, `DELETE FROM report_previews p WHERE p.id IN (
		SELECT id FROM report_previews WHERE created_at < $1
		ORDER BY created_at,id LIMIT $2 FOR UPDATE SKIP LOCKED
	)`, now.UTC().Add(-RetentionPeriod), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
