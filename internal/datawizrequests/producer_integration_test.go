package datawizrequests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"energylens.local/cloud/internal/datawizfacts"
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
	if count, err := p.RunOnce(ctx); err != nil || count != 10 {
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
	if count, err := p.RunOnce(ctx); err != nil || count != 3 {
		t.Fatalf("updated pass: %d %v", count, err)
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
}
