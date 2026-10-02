package reports

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReportRetentionRemovesOldSnapshotsAndRecipients(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "report_retention_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, home := uuid.New(), uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'home','UTC')`, home, owner); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-RetentionPeriod)
	locked, old, boundary := uuid.New(), uuid.New(), uuid.New()
	for index, row := range []struct {
		id      uuid.UUID
		created time.Time
	}{
		{locked, cutoff.Add(-2 * time.Hour)},
		{old, cutoff.Add(-time.Hour)},
		{boundary, cutoff},
	} {
		if _, err = pool.Exec(ctx, `INSERT INTO report_previews(id,owner_user_id,home_id,home_version,idempotency_key,request_hash,schema_version,payload,created_at,expires_at)
			VALUES($1,$2,$3,1,$4,$5,'manual-preview/v1','{}',$6,$7)`, row.id, owner, home, uuid.New(), make([]byte, 32), row.created, row.created.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO report_delivery_requests(owner_user_id,home_id,preview_id,idempotency_key,request_hash,recipient_hash,recipients,approved_snapshot_sha256,created_at)
			VALUES($1,$2,$3,$4,$5,$6,ARRAY['private@example.test'],repeat('a',64),$7)`, owner, home, row.id, uuid.New(), make([]byte, 32), bytes.Repeat([]byte{byte(index + 1)}, 32), row.created); err != nil {
			t.Fatal(err)
		}
	}
	job := RetentionJob{DB: pool}
	if _, err = job.RunOnce(ctx, now, 0); err == nil {
		t.Fatal("zero batch limit accepted")
	}
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	if _, err = lockTx.Exec(ctx, `SELECT id FROM report_previews WHERE id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}
	if deleted, err := job.RunOnce(ctx, now, 1); err != nil || deleted != 1 {
		t.Fatalf("bounded pass with locked preview: deleted=%d err=%v", deleted, err)
	}
	var remaining int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM report_previews WHERE id=$1`, old).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unlocked old preview remained: count=%d err=%v", remaining, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM report_delivery_requests WHERE preview_id=$1`, old).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("recipient row remained after cascade: count=%d err=%v", remaining, err)
	}
	if err = lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if deleted, err := job.RunOnce(ctx, now, 500); err != nil || deleted != 1 {
		t.Fatalf("second pass: deleted=%d err=%v", deleted, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM report_previews`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("30-day boundary preview removed: count=%d err=%v", remaining, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM report_delivery_requests`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("30-day boundary delivery removed: count=%d err=%v", remaining, err)
	}
}
