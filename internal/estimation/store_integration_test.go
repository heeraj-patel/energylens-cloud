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
		Value    string `json:"value"`
		RateKind string `json:"rate_kind"`
	} `json:"cost"`
	WasteEnergy *struct {
		Value string `json:"value"`
	} `json:"waste_energy"`
	WasteCost *struct {
		Value string `json:"value"`
	} `json:"waste_cost"`
	Waste      string `json:"waste"`
	WasteBasis string `json:"waste_basis"`
	RateBasis  string `json:"rate_basis"`
	Quality    struct {
		Consumption string `json:"consumption"`
		Source      string `json:"source"`
		Rate        string `json:"rate"`
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

func estimationTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
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

func TestEstimateUsesResolutionAndTariff(t *testing.T) {
	ctx, pool := estimationTestPool(t)
	var err error
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

func TestEstimateFloorWasteAndNationalRate(t *testing.T) {
	ctx, pool := estimationTestPool(t)
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Fridge','refrigerator','{"brand":"Unknown","full_model":"Unknown","fuel":"electricity","match_quality":"unresolved"}')`, []any{appliance, home}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	setRecord := func(record string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,clock_timestamp()) ON CONFLICT(appliance_ref) DO UPDATE SET record=EXCLUDED.record,resolved_at=clock_timestamp()+interval '1 second'`, "cloud:"+appliance, make([]byte, 32), record); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	rate := 0.2
	store := PGRunStore{Pool: pool, NationalRate: func(context.Context) (float64, error) { return rate, nil }}
	read := func() estimateBody {
		t.Helper()
		runs, err := store.ListRuns(ctx, owner, home, &appliance, now)
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs %d: %v", len(runs), err)
		}
		return decodeEstimate(t, runs[0])
	}
	setRecord(`{"appliance_version":1,"status":"unresolved","device_kind":"real_non_metering","values":null,"source":null,"waste_floor":{"usd_per_day":0.01,"size_class":"large","version":"waste-floor/v2"}}`)
	if got := read(); got.Kind != "unavailable" || got.UnavailableReason != "no_energy_data" || got.Energy != nil {
		t.Fatalf("unresolved record: %+v", got)
	}
	setRecord(`{"schema_version":"datawiz.energy-resolution.v2","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":1.0,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.01,"size_class":"large","version":"waste-floor/v2"}}`)
	got := read()
	// 31 days in October: 31 kWh used, 31 x (0.01 / 0.2) kWh of waste.
	if got.Kind != "estimated_operating_cost" || got.UnavailableReason != "" || got.RateBasis != "us_average" || got.Quality.Rate != "regional_average" || got.Cost.RateKind != "regional_average" || got.Cost.Value != "6.200000" {
		t.Fatalf("no-tariff estimate: %+v", got)
	}
	if got.Waste != "assessed" || got.WasteBasis != "floor" || got.WasteEnergy == nil || got.WasteEnergy.Value != "1.550000" || got.WasteCost.Value != "0.310000" {
		t.Fatalf("no-tariff waste: %+v", got)
	}
	if len(got.EvidenceRefs) != 2 || got.EvidenceRefs[0].Kind != "datawiz_resolution" || got.EvidenceRefs[1].Kind != "national_rate" || got.EvidenceRefs[1].ID != appliance {
		t.Fatalf("no-tariff refs: %+v", got.EvidenceRefs)
	}
	rate = 0.25
	if changed := read(); changed.ID == got.ID || changed.Cost.Value != "7.750000" {
		t.Fatalf("national rate change reused the run: %+v", changed)
	}
	store.NationalRate = func(context.Context) (float64, error) { return 0, errors.New("rate source down") }
	if down := read(); down.Kind != "unavailable" || down.UnavailableReason != "rate_unavailable" {
		t.Fatalf("national rate outage: %+v", down)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.40,'kWh','USD','regional_average','state average',$2,true,$3)`, home, now, now.AddDate(0, -2, 0)); err != nil {
		t.Fatal(err)
	}
	if fallback := read(); fallback.Kind != "unavailable" || fallback.UnavailableReason != "rate_unavailable" {
		t.Fatalf("stored regional fallback tariff must not be the home rate: %+v", fallback)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,2,'electricity',0.10,'kWh','USD','user_tariff','bill',$2,false,$3)`, home, now, now.AddDate(0, -2, 0)); err != nil {
		t.Fatal(err)
	}
	tariff := read()
	if tariff.RateBasis != "home_tariff" || tariff.Quality.Rate != "user_tariff" || tariff.Cost.RateKind != "user_tariff" || tariff.Cost.Value != "3.100000" || tariff.WasteEnergy.Value != "3.100000" || tariff.WasteCost.Value != "0.310000" {
		t.Fatalf("tariff estimate: %+v", tariff)
	}
	if len(tariff.EvidenceRefs) != 2 || tariff.EvidenceRefs[1].Kind != "tariff" {
		t.Fatalf("tariff refs: %+v", tariff.EvidenceRefs)
	}
	capped := `{"schema_version":"datawiz.energy-resolution.v2","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.01,"waste_kwh_per_day":null,"waste_basis":"floor"},"waste_floor":{"usd_per_day":0.01,"size_class":"large","version":"waste-floor/v2"}}`
	setRecord(capped)
	if got = read(); got.WasteEnergy == nil || got.WasteEnergy.Value != got.Energy.Value {
		t.Fatalf("floor waste above usage: %+v", got)
	}
}

func TestGasApplianceLinkNeverYieldsElectricEstimate(t *testing.T) {
	ctx, pool := estimationTestPool(t)
	owner, home, appliance, product := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Acme','GD100A','natural_gas','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO appliances(id,home_id,name,category,catalog_product_id,identity) VALUES($1,$2,'Dryer','dryer',$3,'{"brand":"Acme","full_model":"GD100A","fuel":"natural_gas","match_quality":"confirmed_exact"}')`, []any{appliance, home, product}},
		{`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','bill',$2,false,$3)`, []any{home, now, now.AddDate(0, -1, -1)}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,clock_timestamp())`, []any{"cloud:" + appliance, make([]byte, 32), `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":2.5,"waste_kwh_per_day":0.5}}`}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := PGRunStore{Pool: pool}.ListRuns(ctx, owner, home, &appliance, now)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %d: %v", len(runs), err)
	}
	got := decodeEstimate(t, runs[0])
	if got.Kind != "unavailable" || got.UnavailableReason != "basis_incomparable" || got.Energy != nil || got.Cost != nil || got.WasteEnergy != nil {
		t.Fatalf("gas appliance got an electric estimate: %+v", got)
	}
}
