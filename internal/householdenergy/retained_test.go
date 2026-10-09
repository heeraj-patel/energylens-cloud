package householdenergy

import (
	"context"
	"encoding/json"
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

func TestRetainedHourlyCrossoverAndCorrection(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "retained_energy_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var owner, home, hub, sensor, appliance, attachment string
	for _, step := range []struct {
		query string
		args  []any
		out   *string
	}{
		{`INSERT INTO users DEFAULT VALUES RETURNING id`, nil, &owner},
		{`INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Retained','America/Chicago') RETURNING id`, []any{&owner}, &home},
	} {
		args := make([]any, len(step.args))
		for i, arg := range step.args {
			if p, ok := arg.(*string); ok {
				args[i] = *p
			} else {
				args[i] = arg
			}
		}
		if err = pool.QueryRow(ctx, step.query, args...).Scan(step.out); err != nil {
			t.Fatal(err)
		}
	}
	if err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,$2,'test',$3,1) RETURNING id`, "elhub_"+strings.ReplaceAll(uuid.NewString(), "-", ""), []byte(uuid.NewString()[:32]), home).Scan(&hub); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,1,'meter','plug') RETURNING id`, home, hub).Scan(&sensor); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Metered plug','plug','{}') RETURNING id`, home).Scan(&appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	hour := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	if err = pool.QueryRow(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,1,$4) RETURNING id`, home, sensor, appliance, hour.Add(-time.Hour)).Scan(&attachment); err != nil {
		t.Fatal(err)
	}
	insertObservation := func(event string, revision int64, start time.Time, wh int) {
		t.Helper()
		batch := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,1,$2,$3,$4)`, hub, batch, home, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,1,$2,$3,$4,$5,'energy_interval',$6,$7)`, hub, event, revision, home, sensor, make([]byte, 32), batch); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel) VALUES($1,1,$2,$3,'energy_interval',$4,$5,$6,600,$7,'electricity')`, hub, event, revision, start, start.Add(10*time.Minute), wh, attachment); err != nil {
			t.Fatal(err)
		}
	}
	first, second := uuid.NewString(), uuid.NewString()
	insertObservation(first, 1, hour.Add(10*time.Minute), 100)
	insertObservation(second, 1, hour.Add(30*time.Minute), 200)
	refs, _ := json.Marshal([]retainedRef{{first, 1}, {second, 1}})
	if _, err = pool.Exec(ctx, `INSERT INTO energy_hourly(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,1,$3,$4,$5,300,1200,2,'complete','exact',$6)`, home, hub, sensor, attachment, hour, refs); err != nil {
		t.Fatal(err)
	}
	p := period{Start: hour, End: hour.Add(time.Hour)}
	read := func() []interval {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		raw, _, err := readIntervals(ctx, tx, home, p, p.End.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		items, err := mergeRetainedIntervals(ctx, tx, home, p, raw)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	assertEnergy := func(want string) {
		t.Helper()
		b := makeBuckets(p, "hour", read())
		if b[0].Allocation.Energy == nil || b[0].Allocation.Energy.Value != want {
			t.Fatalf("energy = %+v, want %s", b[0].Allocation.Energy, want)
		}
	}
	assertEnergy("0.300000000000")
	if _, err = pool.Exec(ctx, `DELETE FROM energy_observations WHERE hub_id=$1 AND ownership_epoch=1 AND event_id=$2 AND revision=1`, hub, first); err != nil {
		t.Fatal(err)
	}
	assertEnergy("0.300000000000")
	if _, err = pool.Exec(ctx, `UPDATE energy_hourly SET status='overlap_conflict',energy_wh=NULL,covered_seconds=NULL WHERE home_id=$1`, home); err != nil {
		t.Fatal(err)
	}
	if b := makeBuckets(p, "hour", read()); b[0].Allocation.Energy != nil {
		t.Fatal("conflicting retained hour became measured energy")
	}
	if _, err = pool.Exec(ctx, `UPDATE energy_hourly SET status='complete',energy_wh=300,covered_seconds=1200 WHERE home_id=$1`, home); err != nil {
		t.Fatal(err)
	}
	insertObservation(first, 2, hour.Add(10*time.Minute), 150)
	if b := makeBuckets(p, "hour", read()); b[0].Allocation.Energy != nil {
		t.Fatal("stale retained hour survived a late correction")
	}
	if _, err = pool.Exec(ctx, `UPDATE energy_hourly SET energy_wh=350,source_refs=$2 WHERE home_id=$1`, home, mustJSONRefs(t, []retainedRef{{first, 2}, {second, 1}})); err != nil {
		t.Fatal(err)
	}
	assertEnergy("0.350000000000")
}

func mustJSONRefs(t *testing.T, refs []retainedRef) []byte {
	t.Helper()
	b, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRetainedHourDoesNotSplitAcrossLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	p := period{Start: start, End: start.AddDate(0, 0, 1)}
	// The UTC summary hour 18:00–19:00 straddles local midnight at 18:30.
	v := interval{Start: start.Add(-30 * time.Minute), End: start.Add(30 * time.Minute), Wh: mustDecimal("100"), Summarized: true}
	b := makeBuckets(p, "day", []interval{v})
	if b[0].Allocation.Energy != nil {
		t.Fatal("a retained UTC hour was interpolated across the local day boundary")
	}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	dst := periodFor(time.Date(2026, 3, 8, 18, 0, 0, 0, time.UTC), chicago, "hour")
	if len(makeBuckets(dst, "hour", nil)) != 23 {
		t.Fatal("retained history changed DST bucket count")
	}
}
