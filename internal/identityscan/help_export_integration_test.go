package identityscan

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGHelpExportReadsUnresolvedAndCategoryTypicalResolutions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "helpexport_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, home := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO users(id) VALUES($1)`, owner)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	add := func(brand, model, status, tier string, archived bool) string {
		id := uuid.NewString()
		identity, _ := json.Marshal(map[string]string{"brand": brand, "full_model": model, "fuel": "electricity"})
		exec(`INSERT INTO appliances(id,home_id,name,category,identity,archived_at) VALUES($1,$2,'Washer','washer',$3::jsonb,CASE WHEN $4 THEN now() END)`, id, home, string(identity), archived)
		record := map[string]any{"status": status, "identity": map[string]string{"manufacturer": brand, "model": model}, "source": nil}
		if tier != "" {
			record["source"] = map[string]string{"tier": tier}
		}
		raw, _ := json.Marshal(record)
		exec(`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES('cloud:'||$1,decode(repeat('00',32),'hex'),$2::jsonb,now())`, id, string(raw))
		return id
	}
	unresolved := add("LG", "WM3070HWA", "unresolved", "", false)
	typical := add("Acme", "A-1", "resolved", "category_typical", false)
	add("Acme", "B-2", "resolved", "energy_star", false)
	add("Acme", "C-3", "unresolved", "", true)
	researched := add("Samsung", "WF 45", "unresolved", "", false)

	run := uuid.NewString()
	exec(`INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,'catalog','{}','batch','complete')`, run)
	obs := uuid.NewString()
	exec(`INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at)
VALUES($1,$2,'device','device:samsung:wf45','energy_rating','manual_research:annual_kwh','{}','{"adapter":"manual_research"}','approved',now())`, obs, run)
	exec(`INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES('device','device:samsung:wf45','energy_rating','manual_research:annual_kwh',$1)`, obs)

	path := filepath.Join(t.TempDir(), "unresolved_device_help.jsonl")
	count, err := ExportHelp(ctx, pool, path)
	if err != nil || count != 2 {
		t.Fatalf("count %d err %v", count, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]HelpRecord{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r HelpRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		got[r.ApplianceID] = r
	}
	if r := got[unresolved]; r.Manufacturer != "LG" || r.Model != "WM3070HWA" || r.Category != "washer" || r.Fuel != "electricity" ||
		r.LookupStatus != "unresolved" || r.NormalizedIdentity != "washer|lg|wm3070hwa|electricity" || r.LookupDate.IsZero() {
		t.Fatalf("unresolved record: %+v", r)
	}
	if r := got[typical]; r.LookupStatus != "category_typical" || r.Model != "A-1" {
		t.Fatalf("category typical record: %+v", r)
	}
	if _, ok := got[researched]; ok || len(got) != 2 {
		t.Fatalf("researched, resolved or archived appliances exported: %+v", got)
	}
}
