package recommendation

import (
	"context"
	"fmt"
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

func gasTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_gas_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	return pool
}

func TestCurrentLoadsGasFactsAndCostForGasRules(t *testing.T) {
	pool := gasTestPool(t)
	ctx := context.Background()
	owner, home, furnace, heater, fProduct, wProduct := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := strings.Repeat("d", 64)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v: %s", err, q)
		}
	}
	n := 0
	fact := func(product, kind, unit string, value float64, basis string) {
		t.Helper()
		n++
		exec(`INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
		 VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,'{}','other_official','https://example.org/f',$8,'reviewed_row_manifest','m',$9,1,'{}')`,
			uuid.NewString(), product, hash, kind, value, unit, basis, fmt.Sprintf("%064x", n), now)
	}
	exec(`INSERT INTO users(id) VALUES($1)`, owner)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	exec(`INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',2,3)`, hash)
	exec(`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'furnace','Acme','F1','natural_gas','confirmed_exact','{}'),($2,'water_heater','Acme','W1','natural_gas','confirmed_exact','{}')`, fProduct, wProduct)
	fact(fProduct, "afue", "percent", 96, "{}")
	fact(wProduct, "uniform_energy_factor", "ratio", 0.81, `{"draw_pattern":"Medium"}`)
	exec(`INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES
	 ($1,$2,'Furnace','furnace','{"brand":"Acme","full_model":"F1","fuel":"natural_gas","match_quality":"confirmed_exact"}',$3),
	 ($4,$2,'Water heater','water_heater','{"brand":"Acme","full_model":"W1","fuel":"natural_gas","match_quality":"confirmed_exact"}',$5)`, furnace, home, fProduct, heater, wProduct)
	exec(`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
	 VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','bill',$2,false,$3),($1,2,'natural_gas',1.20,'therm','USD','user_tariff','bill',$2,false,$3)`, home, now, now.Add(-time.Hour))
	store := PGStore{Pool: pool}
	read := func() map[string]Action {
		t.Helper()
		items, err := store.Current(ctx, owner, home, now)
		if err != nil || len(items) != 2 {
			t.Fatalf("current: %+v %v", items, err)
		}
		out := map[string]Action{}
		for _, item := range items {
			for _, action := range EvaluateGasAppliances(item) {
				out[action.RuleID] = action
			}
		}
		return out
	}
	got := read()
	if a := got["furnace.gas_efficiency_check"]; a.State != "active" || a.Explanation != "This furnace is rated 96% AFUE. ENERGY STAR gas furnaces are 95% or higher (97% in northern states)." {
		t.Fatalf("afue: %+v", a)
	}
	if a := got["furnace.gas_standby"]; a.State != "active" || !strings.HasSuffix(a.Explanation, "about $19/yr.") {
		t.Fatalf("typical standby: %+v", a)
	}
	if a := got["water_heater.gas_efficiency_check"]; a.State != "active" || !strings.Contains(a.Explanation, "Rated 0.81 UEF.") {
		t.Fatalf("uef: %+v", a)
	}
	if a := got["water_heater.gas_cost"]; a.NextQuestion != "household_size" {
		t.Fatalf("cost before answer: %+v", a)
	}
	exec(`INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'water_heater.household_size',2,1,'confirmed','64',$3)`, home, heater, now)
	therms := (365*64*8.24*67/0.81 - 3412*29) / 100000
	want := fmt.Sprintf("Heating water costs about $%.0f/yr with your gas rate.", therms*1.2)
	if a := read()["water_heater.gas_cost"]; a.State != "active" || a.Explanation != want || len(a.EvidenceRefs) != 3 || a.SavingsAmount != nil {
		t.Fatalf("cost: %+v want %s", a, want)
	}
	exec(`INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status) VALUES($1,$2,1,'water_heater.household_size',1,2,'unknown')`, home, heater)
	if a := read()["water_heater.gas_cost"]; a.State != "active" || len(a.EvidenceRefs) != 2 {
		t.Fatalf("not sure: %+v", a)
	}
	exec(`UPDATE appliances SET identity='{"brand":"Acme","full_model":"W1","fuel":"propane","match_quality":"confirmed_exact"}' WHERE id=$1`, heater)
	if a := read()["water_heater.gas_cost"]; a.State != "candidate" || a.NextQuestion != "find_reviewed_energy_source" {
		t.Fatalf("propane with mismatched catalog fuel: %+v", a)
	}
	// Country gate: the water temperature tip needs a US location revision.
	exec(`UPDATE appliances SET identity='{"brand":"Acme","full_model":"W1","fuel":"natural_gas","match_quality":"confirmed_exact"}' WHERE id=$1`, heater)
	if _, ok := read()["water_heater.gas_temperature"]; ok {
		t.Fatal("gas_temperature shown with no home location")
	}
	exec(`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','60601','postal_code','user_entered')`, home)
	if a, ok := read()["water_heater.gas_temperature"]; !ok || a.State != "active" {
		t.Fatalf("gas_temperature for US home: %+v %v", a, ok)
	}
	exec(`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'CA','M5V','postal_code','user_entered')`, home)
	if _, ok := read()["water_heater.gas_temperature"]; ok {
		t.Fatal("gas_temperature shown for CA home")
	}
}
