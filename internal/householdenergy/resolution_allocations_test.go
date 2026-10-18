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

func TestResolvedEnergyAcrossDSTDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	// The local day has 23 clock hours; its daily estimate still sums to one day.
	var full resolvedEnergy
	if err := json.Unmarshal([]byte(`{"values":{"kwh_per_day":0.024,"waste_kwh_per_day":0.024}}`), &full); err != nil {
		t.Fatal(err)
	}
	energy, waste := resolutionKWhForPeriod(full, start, start.AddDate(0, 0, 1))
	if energy < 0.023999 || energy > 0.024001 || waste != energy {
		t.Fatalf("DST day: %f/%f", energy, waste)
	}
}

func TestResolutionAllocationsExcludeMeteringAttachment(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "resolution_alloc_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, appliance, hub, sensor := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"fuel":"electricity","match_quality":"unresolved"}')`, []any{appliance, home}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, []any{"cloud:" + appliance, make([]byte, 32), `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.008,"waste_kwh_per_day":0.008}}`}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	loc, _ := time.LoadLocation("America/Chicago")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	buckets := []bucket{{Start: start, End: start.AddDate(0, 0, 1)}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, period, err := resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1))
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 || len(period) != 1 || period[0].Allocation.WasteEnergy == nil {
		t.Fatalf("nonmetering allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"New","full_model":"M2","fuel":"electricity","match_quality":"unresolved"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1))
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("stale identity allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE datawiz_energy_resolutions SET resolved_at=clock_timestamp()+interval '1 hour' WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1))
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("late old result allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=jsonb_set(record,'{appliance_version}','2'::jsonb) WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1))
	if err != nil || len(rows[0]) != 1 || len(period) != 1 {
		t.Fatalf("current identity allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_resolution_test',$2,'test',$3,1)`, hub, make([]byte, 32), home); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,$3,1,'meter','plug',ARRAY['power','energy'])`, sensor, home, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,2,'{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,2,$4)`, home, sensor, appliance, start); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1))
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("metering got estimate: %+v %+v %v", rows, period, err)
	}
}
