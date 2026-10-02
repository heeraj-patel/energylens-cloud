package meteredrollup

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRollupCorrectionAndRetentionGate(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "rollup_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "UTC", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var hub, sensor, appliance, attachment string
	if err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,$2,'test',$3,2) RETURNING id`, "elhub_"+uuid.NewString(), make([]byte, 32), home.ID).Scan(&hub); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,2,'meter','plug',ARRAY['power','energy']) RETURNING id`, home.ID, hub).Scan(&sensor); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Device','refrigerator','{}') RETURNING id`, home.ID).Scan(&appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-35 * 24 * time.Hour).Add(-30 * time.Minute)
	end := start.Add(time.Hour)
	if err = pool.QueryRow(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,1,$4) RETURNING id`, home.ID, sensor, appliance, start.Add(-time.Hour)).Scan(&attachment); err != nil {
		t.Fatal(err)
	}
	intervalID, pointID, gapID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	insertBatch := func(revision int64, batchID string, intervalStart time.Time, wh float64) {
		t.Helper()
		if _, err = pool.Exec(ctx, `INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,2,$2,$3,$4)`, hub, batchID, home.ID, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,2,$2,$3,$4,$5,'energy_interval',$6,$7)`, hub, intervalID, revision, home.ID, sensor, make([]byte, 32), batchID); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id) VALUES($1,2,$2,$3,'energy_interval',$4,$5,$6,3600,$7)`, hub, intervalID, revision, intervalStart, intervalStart.Add(time.Hour), wh, attachment); err != nil {
			t.Fatal(err)
		}
	}
	batch1 := uuid.NewString()
	insertBatch(1, batch1, start, 60)
	if _, err = pool.Exec(ctx, `INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,2,$2,1,$3,$4,'power_point',$5,$6),($1,2,$7,1,$3,$4,'coverage_gap',$5,$6)`, hub, pointID, home.ID, sensor, make([]byte, 32), batch1, gapID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,observed_at,watts) VALUES($1,2,$2,1,'power_point',$3,120)`, hub, pointID, start); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO coverage_gaps(hub_id,ownership_epoch,event_id,revision,first_at,last_at,reason,dropped_count) VALUES($1,2,$2,1,$3,$4,'offline',3)`, hub, gapID, start, end); err != nil {
		t.Fatal(err)
	}
	job := Job{Pool: pool}
	run := func() Stats {
		t.Helper()
		s, e := job.RunOnce(ctx, now, 10, 200, 100)
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	if s := run(); s.BatchesScanned != 1 || s.BucketsRecomputed == 0 || s.DetailRowsPurged != 0 {
		t.Fatalf("initial run: %+v", s)
	}
	var sum float64
	if err = pool.QueryRow(ctx, `SELECT sum(energy_wh) FROM energy_hourly WHERE home_id=$1 AND status='complete'`, home.ID).Scan(&sum); err != nil || sum != 60 {
		t.Fatalf("hourly conservation: %g %v", sum, err)
	}
	if err = pool.QueryRow(ctx, `SELECT sum(energy_wh) FROM energy_daily WHERE home_id=$1 AND status='complete'`, home.ID).Scan(&sum); err != nil || sum != 60 {
		t.Fatalf("daily conservation: %g %v", sum, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT sample_count FROM power_hourly WHERE home_id=$1 AND bucket_start=$2`, home.ID, start.Truncate(time.Hour)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("point summary: %d %v", count, err)
	}
	if err = pool.QueryRow(ctx, `SELECT gap_overlap_count FROM coverage_hourly WHERE home_id=$1 AND bucket_start=$2`, home.ID, start.Truncate(time.Hour)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("gap coverage: %d %v", count, err)
	}
	// A revision moves the interval; both old and new buckets must be recalculated.
	batch2 := uuid.NewString()
	insertBatch(2, batch2, start.Add(2*time.Hour), 90)
	if s := run(); s.BatchesScanned != 1 {
		t.Fatalf("correction run: %+v", s)
	}
	if err = pool.QueryRow(ctx, `SELECT sum(energy_wh) FROM energy_hourly WHERE home_id=$1 AND status='complete'`, home.ID).Scan(&sum); err != nil || sum != 90 {
		t.Fatalf("corrected hourly: %g %v", sum, err)
	}
	var oldStatus string
	if err = pool.QueryRow(ctx, `SELECT status FROM energy_hourly WHERE home_id=$1 AND bucket_start=$2`, home.ID, start.Truncate(time.Hour)).Scan(&oldStatus); err != nil || oldStatus != "no_intervals" {
		t.Fatalf("old correction bucket: %s %v", oldStatus, err)
	}
	// Simulate a completed correction window. Missing daily point evidence must block only its raw sample.
	if _, err = pool.Exec(ctx, `UPDATE ingest_events SET accepted_at=$1 WHERE home_id=$2`, now.Add(-31*24*time.Hour), home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM power_daily WHERE home_id=$1`, home.ID); err != nil {
		t.Fatal(err)
	}
	s := run()
	if s.DetailRowsPurged == 0 {
		t.Fatalf("other durable details did not purge: %+v", s)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_observations WHERE event_id=$1`, pointID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("point lost without daily summary: %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO metered_rollup_dirty(kind,home_id,hub_id,ownership_epoch,sensor_id,bucket_start) VALUES('power_day',$1,$2,2,$3,$4)`, home.ID, hub, sensor, dayUTC(start)); err != nil {
		t.Fatal(err)
	}
	if s = run(); s.DetailRowsPurged != 1 {
		t.Fatalf("point was not purged after durable summary: %+v", s)
	}
}
