package kasaruntime

import (
	"context"
	"errors"
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

func TestKasaRuntimeRetentionBoundsLocksAndOwnerRead(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "runtime_retention_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, hub, sensor := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_retention_test',$2,'test',$3,1)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,1,'plug','plug')`, []any{sensor, home, hub}},
	} {
		if _, err = pool.Exec(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-RetentionPeriod)
	locked, old, boundary, recent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id       uuid.UUID
		observed time.Time
	}{
		{locked, cutoff.Add(-2 * time.Hour)},
		{old, cutoff.Add(-time.Hour)},
		{boundary, cutoff},
		{recent, now.Add(-30 * 24 * time.Hour)},
	} {
		if _, err = pool.Exec(ctx, `INSERT INTO kasa_runtime_snapshots
			(hub_id,ownership_epoch,snapshot_id,home_id,sensor_id,scope,window_label,native_unit,native_minutes,duration_seconds,device_clock_valid,observed_at,method,transport,quality,payload_hash)
			VALUES($1,1,$2,$3,$4,'single_plug','today','minutes',0,0,false,$5,'get_daystat','kasa_legacy_tcp','protocol_reported_clock_unverified',$6)`, hub, row.id, home, sensor, row.observed, make([]byte, 32)); err != nil {
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
	if _, err = lockTx.Exec(ctx, `SELECT snapshot_id FROM kasa_runtime_snapshots WHERE snapshot_id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}
	if deleted, err := job.RunOnce(ctx, now, 1); err != nil || deleted != 1 {
		t.Fatalf("bounded pass with locked row: deleted=%d err=%v", deleted, err)
	}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kasa_runtime_snapshots WHERE snapshot_id=$1)`, old).Scan(&exists); err != nil || exists {
		t.Fatalf("unlocked old row remained: exists=%v err=%v", exists, err)
	}
	if err = lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if deleted, err := job.RunOnce(ctx, now, 500); err != nil || deleted != 1 {
		t.Fatalf("second pass: deleted=%d err=%v", deleted, err)
	}
	var remaining int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM kasa_runtime_snapshots`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("boundary or recent snapshot removed: count=%d err=%v", remaining, err)
	}
	store := Store{Pool: pool}
	cursor := pageCursor{Home: home.String(), From: cutoff, To: now}
	rows, err := store.Page(ctx, home.String(), owner.String(), cursor, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("owner lost cursor-window rows: rows=%d err=%v", len(rows), err)
	}
	if _, err = store.Page(ctx, home.String(), other.String(), cursor, 10); !errors.Is(err, ErrReference) {
		t.Fatalf("other owner read: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE kasa_runtime_snapshots SET native_minutes=1 WHERE snapshot_id=$1`, recent); err == nil {
		t.Fatal("retained snapshot became mutable")
	}
	v := validSnapshot(cutoff.Add(-time.Hour))
	if v.valid(now) {
		t.Fatal("expired snapshot accepted for replay")
	}
}
