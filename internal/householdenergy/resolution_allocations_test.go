package householdenergy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/estimation"
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

// Both readers must classify the same persisted resolution and effective rate
// the same way over the same local calendar month.
func TestResolutionEstimateAllocationWasteParity(t *testing.T) {
	cases := []struct {
		name, values, floor, basis string
		homeRate                   bool
		known                      bool
	}{
		{"standby/home tariff", `"kwh_per_day":1,"waste_kwh_per_day":0.125`, `,"waste_floor":{"usd_per_day":0.01}`, "standby", true, true},
		{"explicit zero overrides floor", `"kwh_per_day":1,"waste_kwh_per_day":0`, `,"waste_floor":{"usd_per_day":0.01}`, "standby", true, true},
		{"floor/home tariff", `"kwh_per_day":1,"waste_kwh_per_day":null`, `,"waste_floor":{"usd_per_day":0.01}`, "floor", true, true},
		{"floor/national rate", `"kwh_per_day":1,"waste_kwh_per_day":null`, `,"waste_floor":{"usd_per_day":0.01}`, "floor", false, true},
		{"floor capped", `"kwh_per_day":0.01,"waste_kwh_per_day":null`, `,"waste_floor":{"usd_per_day":0.01}`, "floor", true, true},
		{"legacy missing waste", `"kwh_per_day":1`, "", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool := resolutionTestPool(t)
			owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
			now := time.Now().UTC()
			start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0)
			record := fmt.Sprintf(`{"schema_version":"datawiz.energy-resolution.v2","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{%s,"waste_basis":%q}%s}`, tc.values, tc.basis, tc.floor)
			if !tc.known {
				record = fmt.Sprintf(`{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{%s}}`, tc.values)
			}
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
				{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC')`, []any{home, owner}},
				{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"brand":"Unknown","full_model":"Unknown","fuel":"electricity","match_quality":"unresolved"}')`, []any{appliance, home}},
				{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, []any{"cloud:" + appliance, make([]byte, 32), record}},
			} {
				if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
					t.Fatal(err)
				}
			}
			if tc.homeRate {
				_, err := pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,effective_from,source_as_of,is_fallback) VALUES($1,1,'electricity',0.10,'kWh','USD','user_tariff','test',$2,$3,false)`, home, start.AddDate(0, -1, 0), now.Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
			}
			national := func(context.Context) (float64, error) { return 0.2, nil }
			// ListRuns locks the home row, so finish it before the allocation transaction.
			runs, err := (estimation.PGRunStore{Pool: pool, NationalRate: national}).ListRuns(ctx, owner, home, &appliance, now)
			if err != nil || len(runs) != 1 {
				t.Fatalf("estimate runs %d: %v", len(runs), err)
			}
			var estimate struct {
				Waste       string `json:"waste"`
				WasteBasis  string `json:"waste_basis"`
				WasteEnergy *struct {
					Value string `json:"value"`
				} `json:"waste_energy"`
				WasteCost *struct {
					Value string `json:"value"`
				} `json:"waste_cost"`
			}
			if err := json.Unmarshal(runs[0], &estimate); err != nil {
				t.Fatal(err)
			}
			buckets := make([]bucket, 0, end.Sub(start)/(24*time.Hour))
			for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
				buckets = append(buckets, bucket{Start: day, End: day.AddDate(0, 0, 1)})
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, period, err := resolutionDeviceRows(ctx, tx, home, buckets, now, NationalRate(national))
			if err != nil || len(period) != 1 {
				t.Fatalf("allocation rows %d: %v", len(period), err)
			}
			a := period[0].Allocation
			if tc.known {
				if estimate.Waste != "assessed" || estimate.WasteBasis != tc.basis || a.WasteBasis != tc.basis || estimate.WasteEnergy == nil || estimate.WasteCost == nil || a.WasteEnergy == nil || a.WasteCost == nil {
					t.Fatalf("waste markers/amounts: estimate %+v, allocation %+v", estimate, a)
				}
				if got := amount(t, a.WasteEnergy).FloatString(6); got != estimate.WasteEnergy.Value {
					t.Fatalf("waste energy allocation %s, estimate %s", got, estimate.WasteEnergy.Value)
				}
				if got := amount(t, a.WasteCost).FloatString(6); got != estimate.WasteCost.Value {
					t.Fatalf("waste cost allocation %s, estimate %s", got, estimate.WasteCost.Value)
				}
			} else if estimate.Waste != "not_yet_assessed" || estimate.WasteBasis != "" || estimate.WasteEnergy != nil || estimate.WasteCost != nil || a.WasteBasis != "" || a.WasteEnergy != nil || a.WasteCost != nil || a.UnclassifiedEnergy == nil || a.UnclassifiedCost == nil {
				t.Fatalf("legacy unknown classification: estimate %+v, allocation %+v", estimate, a)
			}
		})
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
