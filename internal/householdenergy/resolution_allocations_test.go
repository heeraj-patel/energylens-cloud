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
	energy, waste, _ := resolutionKWhForPeriod(full, start, start.AddDate(0, 0, 1), 0)
	if energy < 0.023999 || energy > 0.024001 || waste != energy {
		t.Fatalf("DST day: %f/%f", energy, waste)
	}
}

func resolutionTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
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
	t.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return ctx, pool
}

func TestResolutionAllocationsExcludeMeteringAttachment(t *testing.T) {
	ctx, pool := resolutionTestPool(t)
	var err error
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
	rows, period, err := resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1), nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 || len(period) != 1 || period[0].Allocation.WasteEnergy == nil {
		t.Fatalf("nonmetering allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"New","full_model":"M2","fuel":"electricity","match_quality":"unresolved"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1), nil)
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("stale identity allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE datawiz_energy_resolutions SET resolved_at=clock_timestamp()+interval '1 hour' WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1), nil)
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("late old result allocation: %+v %+v %v", rows, period, err)
	}
	if _, err = tx.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=jsonb_set(record,'{appliance_version}','2'::jsonb) WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1), nil)
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
	rows, period, err = resolutionDeviceRows(ctx, tx, home, buckets, start.AddDate(0, 0, 1), nil)
	if err != nil || len(rows[0]) != 0 || len(period) != 0 {
		t.Fatalf("metering got estimate: %+v %+v %v", rows, period, err)
	}
}

func TestResolutionAllocationsFloorWasteUsesHomeOrNationalRate(t *testing.T) {
	ctx, pool := resolutionTestPool(t)
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	record := `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":1.32,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.01,"size_class":"large","version":"waste-floor/v2"}}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Fridge','refrigerator','{"fuel":"electricity","match_quality":"unresolved"}')`, []any{appliance, home}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, []any{"cloud:" + appliance, make([]byte, 32), record}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 2)
	buckets := []bucket{{Start: start, End: start.AddDate(0, 0, 1)}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	national := NationalRate(func(context.Context) (float64, error) { return 0.2, nil })
	_, period, err := resolutionDeviceRows(ctx, tx, home, buckets, now, national)
	if err != nil || len(period) != 1 {
		t.Fatalf("no tariff: %+v %v", period, err)
	}
	a := period[0].Allocation
	if a.RateBasis != "us_average" || a.WasteBasis != "floor" || a.EvidenceBasis != "estimated" {
		t.Fatalf("no tariff markers: %+v", a)
	}
	if got := amount(t, a.WasteEnergy).FloatString(4); got != "0.0500" {
		t.Fatalf("no tariff waste kWh = %s", got)
	}
	if amount(t, a.Cost).FloatString(4) != "0.2640" || amount(t, a.WasteCost).FloatString(4) != "0.0100" || amount(t, a.UsefulCost).FloatString(4) != "0.2540" {
		t.Fatalf("no tariff costs: %+v", a)
	}
	if _, period, err = resolutionDeviceRows(ctx, tx, home, buckets, now, nil); err != nil || len(period) != 1 {
		t.Fatalf("no rate: %+v %v", period, err)
	}
	if a = period[0].Allocation; a.Cost != nil || a.WasteEnergy != nil || a.RateBasis != "" {
		t.Fatalf("unpriced row: %+v", a)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,effective_from,source_as_of,is_fallback) VALUES($1,1,'electricity',0.1,'kWh','USD','user_tariff','test',$2,$3,false)`, home, start.AddDate(0, -1, 0), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, period, err = resolutionDeviceRows(ctx, tx, home, buckets, now, national)
	if err != nil || len(period) != 1 {
		t.Fatalf("home tariff: %+v %v", period, err)
	}
	a = period[0].Allocation
	if a.RateBasis != "home_tariff" || amount(t, a.WasteEnergy).FloatString(4) != "0.1000" || amount(t, a.WasteCost).FloatString(4) != "0.0100" {
		t.Fatalf("home tariff row: %+v", a)
	}
}

func TestResolutionAllocationsSkipGasAppliances(t *testing.T) {
	ctx, pool := resolutionTestPool(t)
	owner, home, gas, electric := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	record := `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.008,"waste_kwh_per_day":0.008}}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Gas dryer','dryer','{"fuel":"natural_gas","match_quality":"confirmed_exact"}')`, []any{gas, home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"fuel":"electricity","match_quality":"unresolved"}')`, []any{electric, home}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now()),($4,$2,$3,now())`, []any{"cloud:" + gas, make([]byte, 32), record, "cloud:" + electric}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	loc, _ := time.LoadLocation("America/Chicago")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, period, err := resolutionDeviceRows(ctx, tx, home, []bucket{{Start: start, End: start.AddDate(0, 0, 1)}}, start.AddDate(0, 0, 1), nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 || len(period) != 1 || rows[0][0].ApplianceID != electric {
		t.Fatalf("only the electric appliance may be allocated: %+v %+v %v", rows, period, err)
	}
}
