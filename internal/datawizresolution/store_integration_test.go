package datawizresolution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResolutionOwnerAndHubScope(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawizresolution_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, hub, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_resolution_scope',$2,'test',$3,1)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	if _, err = store.ForOwner(ctx, other, home, appliance); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err = store.ForOwner(ctx, owner, home, appliance); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending resolution: %v", err)
	}
	record := `{"schema_version":"datawiz.energy-resolution.v1","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.01}}`
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, "cloud:"+appliance, make([]byte, 32), record); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (json.RawMessage, error){
		func() (json.RawMessage, error) { return store.ForOwner(ctx, owner, home, appliance) },
		func() (json.RawMessage, error) { return store.ForHub(ctx, hub, appliance) },
	} {
		body, e := read()
		if e != nil || !json.Valid(body) || !strings.Contains(string(body), `"kwh_per_day": 0.01`) {
			t.Fatalf("published resolution: %s %v", body, e)
		}
	}
	if _, err = store.ForHub(ctx, uuid.NewString(), appliance); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other hub: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"New","full_model":"M2","fuel":"electricity","match_quality":"unresolved"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (json.RawMessage, error){
		func() (json.RawMessage, error) { return store.ForOwner(ctx, owner, home, appliance) },
		func() (json.RawMessage, error) { return store.ForHub(ctx, hub, appliance) },
	} {
		if _, e := read(); !errors.Is(e, ErrNotFound) {
			t.Fatalf("old identity result shown after edit: %v", e)
		}
	}
	// An old worker can finish after the edit. Its newer timestamp still cannot
	// make the old appliance version visible to either reader.
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET resolved_at=clock_timestamp()+interval '1 hour' WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ForOwner(ctx, owner, home, appliance); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late old result shown: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=jsonb_set(record,'{appliance_version}','2'::jsonb) WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ForOwner(ctx, owner, home, appliance); err != nil {
		t.Fatalf("current version result missing: %v", err)
	}
}
