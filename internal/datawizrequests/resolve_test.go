package datawizrequests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type countingDB struct{ calls int }

func (d *countingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.calls++
	return pgconn.CommandTag{}, nil
}

func TestQueueAcceptsOnlyKnownReasons(t *testing.T) {
	id := uuid.NewString()
	for _, reason := range []string{"usage_answer", "admin", "owner_refresh", "low_tier", "stale", "source_config", "catalog_change", "schema"} {
		db := &countingDB{}
		if err := Queue(context.Background(), db, id, reason); err != nil || db.calls != 1 {
			t.Fatalf("%s: err=%v calls=%d", reason, err, db.calls)
		}
	}
	for _, reason := range []string{"", "manual_research", "Admin"} {
		db := &countingDB{}
		if err := Queue(context.Background(), db, id, reason); err == nil || db.calls != 0 {
			t.Fatalf("%q accepted: err=%v calls=%d", reason, err, db.calls)
		}
	}
}
