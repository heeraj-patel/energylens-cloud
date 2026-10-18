package datawizrequests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/democost/testfixture"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProducerSavedInputsAndRevisions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawizrequests_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','60601','postal_code','user_entered')`, []any{home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'HVAC','hvac','{"brand":"Acme","full_model":"Heat-100","match_quality":"confirmed_exact"}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	p := Producer{DB: pool, Dir: dir}
	if count, err := p.RunOnce(ctx); err != nil || count != 11 {
		t.Fatalf("first pass: %d %v", count, err)
	}
	assertPayload := func(route string, want map[string]any) {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(dir, route, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("%s files: %v %v", route, paths, err)
		}
		body, err := os.ReadFile(paths[len(paths)-1])
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err = json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("%s %s: %v", route, key, got[key])
			}
		}
	}
	assertPayload("device", map[string]any{"manufacturer": "Acme", "model": "Heat-100"})
	assertPayload("resolve", map[string]any{"appliance_ref": "cloud:" + appliance, "appliance_version": float64(1), "device_kind": "real_non_metering", "category": "hvac"})
	assertPayload("zip", map[string]any{"zip": "60601"})
	if count, err := p.RunOnce(ctx); err != nil || count != 0 {
		t.Fatalf("repeat pass: %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"Acme","full_model":"Heat-200"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','02108','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if count, err := p.RunOnce(ctx); err != nil || count != 4 {
		t.Fatalf("updated pass: %d %v", count, err)
	}
	entry, found, err := p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.ApplianceVersion != 2 || entry.entry.Model != "Heat-200" {
		t.Fatalf("edited appliance resolve request: %+v %t %v", entry, found, err)
	}
	if paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json")); len(paths) != 2 {
		t.Fatalf("device revisions: %d", len(paths))
	}
	if paths, _ := filepath.Glob(filepath.Join(dir, "zip", "*.json")); len(paths) != 2 {
		t.Fatalf("ZIP revisions: %d", len(paths))
	}
	runID, observationID := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,'device','{"manufacturer":"Acme","model":"Heat-200"}','realtime','complete')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at)
VALUES($1,$2,'device','device:acme:heat-200','energy_rating','annual','{"amount":"500","unit":"kWh/year"}','{"adapter":"test","url":"https://example.invalid/source","retrieved_at":"2026-10-12T00:00:00Z"}','approved',now())`, observationID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES('device','device:acme:heat-200','energy_rating','annual',$1)`, observationID); err != nil {
		t.Fatal(err)
	}
	facts, err := (datawizfacts.Store{DB: pool}).Current(ctx, "device", "device:acme:heat-200")
	if err != nil || len(facts) != 1 || facts[0].Field != "energy_rating" {
		t.Fatalf("approved current facts: %+v %v", facts, err)
	}
	if err = Queue(ctx, pool, appliance, "admin"); err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			count, e := p.drainQueue(ctx)
			if e != nil {
				results <- -1
				return
			}
			results <- count
		}()
	}
	if a, b := <-results, <-results; a+b != 1 || a < 0 || b < 0 {
		t.Fatalf("concurrent queue drains: %d %d", a, b)
	}
	if err = Queue(ctx, pool, appliance, "admin"); err != nil {
		t.Fatal(err)
	}
	if count, e := p.drainQueue(ctx); e != nil || count != 1 {
		t.Fatalf("replay queue: %d %v", count, e)
	}
	snapshotJSON, err := json.Marshal(testfixture.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET demo_cost_snapshot=$2 WHERE id=$1`, home, snapshotJSON); err != nil {
		t.Fatal(err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 10 {
		t.Fatalf("simulated initial wave: %d %v", count, e)
	}
	demos, err := inventory.DemoAppliances(testfixture.DemoSnapshot(), home)
	if err != nil {
		t.Fatal(err)
	}
	if len(demos) != 10 {
		t.Fatalf("demo appliances: %d", len(demos))
	}
	demoEntry, found, err := p.resolveEntryFor(ctx, demos[0].ID)
	if err != nil || !found || demoEntry.entry.ApplianceVersion != 1 {
		t.Fatalf("simulated resolve version: %+v %t %v", demoEntry, found, err)
	}
	if err = Queue(ctx, pool, demos[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if count, e := p.drainQueue(ctx); e != nil || count != 1 {
		t.Fatalf("simulated on-demand queue: %d %v", count, e)
	}
	hub, sensor, attachment := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_resolve_producer',$2,'test',$3,1)`, hub, make([]byte, 32), home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,$3,1,'meter','plug',ARRAY['power','energy'])`, sensor, home, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,2,'{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,2,now())`, attachment, home, sensor, appliance); err != nil {
		t.Fatal(err)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.DeviceKind != "real_metering" {
		t.Fatalf("metering attachment request: %+v %t %v", entry, found, err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("attachment change request: %d %v", count, e)
	}
	if _, err = pool.Exec(ctx, `UPDATE attachments SET ended_at=now(),version=2 WHERE id=$1`, attachment); err != nil {
		t.Fatal(err)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.DeviceKind != "real_non_metering" {
		t.Fatalf("closed metering attachment request: %+v %t %v", entry, found, err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("attachment closure request: %d %v", count, e)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET name='Renamed HVAC',version=3 WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("name-only version bump did not requeue: %d %v", count, e)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.ApplianceVersion != 3 {
		t.Fatalf("name-only version request: %+v %t %v", entry, found, err)
	}
}
