package estimation

import (
	"context"
	"encoding/json"
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

type estimateBody struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	UnavailableReason string `json:"unavailable_reason"`
	Energy            *struct {
		Value string `json:"value"`
	} `json:"energy"`
	Cost *struct {
		Value string `json:"value"`
	} `json:"cost"`
	WasteEnergy *struct {
		Value string `json:"value"`
	} `json:"waste_energy"`
	Quality struct {
		Consumption string `json:"consumption"`
		Source      string `json:"source"`
	} `json:"quality"`
	EvidenceRefs []struct {
		ID, Kind string
		Revision int64
	} `json:"evidence_refs"`
}

func decodeEstimate(t *testing.T, raw json.RawMessage) estimateBody {
	t.Helper()
	var out estimateBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEstimateUsesResolutionAndTariff(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "estimate_resolution_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"brand":"Unknown","full_model":"Unknown","fuel":"electricity","match_quality":"unresolved"}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	store := PGRunStore{Pool: pool}
	if _, err = store.ListRuns(ctx, other, home, nil, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner isolation: %v", err)
	}
	read := func() estimateBody {
		t.Helper()
		runs, e := store.ListRuns(ctx, owner, home, &appliance, now)
		if e != nil || len(runs) != 1 {
			t.Fatalf("runs %d: %v", len(runs), e)
		}
		return decodeEstimate(t, runs[0])
	}
	initial := read()
	if initial.Kind != "unavailable" || initial.UnavailableReason != "resolution_pending" {
		t.Fatalf("before resolution: %+v", initial)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','bill',$2,false,$3)`, home, now, now.AddDate(0, -1, -1)); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.UnavailableReason != "resolution_pending" {
		t.Fatalf("tariff without record: %+v", got)
	}
	resolved := `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.008,"waste_kwh_per_day":0.008}}`
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,clock_timestamp())`, "cloud:"+appliance, make([]byte, 32), resolved); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.Kind != "estimated_operating_cost" || got.Energy == nil || got.Cost == nil || got.WasteEnergy == nil || got.Energy.Value == "0.000000" || got.Energy.Value != got.WasteEnergy.Value || len(got.EvidenceRefs) != 2 || got.EvidenceRefs[0].Kind != "datawiz_resolution" || got.Quality.Consumption != "resolved" || got.Quality.Source != "datawiz_resolution" {
		t.Fatalf("resolved estimate: %+v", got)
	}
	if again := read(); again.ID != got.ID {
		t.Fatalf("unchanged resolution made new run: %+v", again)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=$2,resolved_at=clock_timestamp()+interval '1 second' WHERE appliance_ref=$1`, "cloud:"+appliance, `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.016,"waste_kwh_per_day":null}}`); err != nil {
		t.Fatal(err)
	}
	changed := read()
	if changed.ID == got.ID || changed.Energy == nil || changed.Energy.Value == got.Energy.Value || changed.WasteEnergy != nil {
		t.Fatalf("resolution change: %+v", changed)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"New","full_model":"M2","fuel":"electricity","match_quality":"unresolved"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	pending := read()
	if pending.ID == changed.ID || pending.Kind != "unavailable" || pending.UnavailableReason != "resolution_pending" || pending.Energy != nil {
		t.Fatalf("old identity estimate shown after edit: %+v", pending)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET resolved_at=clock_timestamp()+interval '1 hour' WHERE appliance_ref=$1`, "cloud:"+appliance); err != nil {
		t.Fatal(err)
	}
	if late := read(); late.Kind != "unavailable" || late.UnavailableReason != "resolution_pending" {
		t.Fatalf("late old result shown: %+v", late)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=$2,resolved_at=clock_timestamp()+interval '2 hours' WHERE appliance_ref=$1`, "cloud:"+appliance, `{"appliance_version":2,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.024}}`); err != nil {
		t.Fatal(err)
	}
	current := read()
	if current.Kind != "estimated_operating_cost" || current.Energy == nil || current.ID == pending.ID {
		t.Fatalf("current identity estimate missing: %+v", current)
	}
	hub, sensor := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_estimation_test',$2,'test',$3,1)`, hub, make([]byte, 32), home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,$3,1,'meter','plug',ARRAY['power','energy'])`, sensor, home, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,2,'{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,2,$4)`, home, sensor, appliance, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	attached := read()
	if attached.Kind != "unavailable" || attached.Energy != nil || attached.ID == current.ID {
		t.Fatalf("metering attachment must override stale resolution: %+v", attached)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET record=$2,resolved_at=clock_timestamp()+interval '3 hours' WHERE appliance_ref=$1`, "cloud:"+appliance, `{"appliance_version":2,"status":"measured_device","device_kind":"real_metering","values":null}`); err != nil {
		t.Fatal(err)
	}
	measured := read()
	if measured.Kind != "unavailable" || measured.Energy != nil || measured.Cost != nil {
		t.Fatalf("metering fallback: %+v", measured)
	}
}
