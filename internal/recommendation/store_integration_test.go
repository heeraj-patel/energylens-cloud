package recommendation

import (
	"context"
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

func TestCurrentRecommendationEvidenceOwnerAndRevisions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, device, product, fact, answer, tariff := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	hash := strings.Repeat("a", 64)
	now := time.Now().UTC()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, []any{hash}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'computer','Acme','A1','electricity','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',20,'kWh','{}','{}','epa_exact','https://example.org/fact',$3,'reviewed_row_manifest','sources/example.json',$4,1,'{}')`, []any{fact, product, hash, now}},
		{`INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES($1,$2,'Computer','computer','{"fuel":"electricity","match_quality":"confirmed_exact"}',$3)`, []any{device, home, product}},
	}
	for _, stmt := range stmts {
		if _, err = pool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	if _, err = store.Current(ctx, other, home, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	check := func(state, next string, refs int) {
		t.Helper()
		items, readErr := store.Current(ctx, owner, home, now)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(items) != 1 {
			t.Fatalf("appliances: %+v", items)
		}
		actions := Evaluate(items[0])
		if len(actions) != 1 || actions[0].State != state || actions[0].NextQuestion != next || len(actions[0].EvidenceRefs) != refs || actions[0].SavingsAmount != nil {
			t.Fatalf("actions: %+v", actions)
		}
	}
	check("candidate", "hours_per_week", 1)
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(id,home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,$3,1,'computer.hours_per_week',1,1,'confirmed','0',$4)`, answer, home, device, now); err != nil {
		t.Fatal(err)
	}
	check("candidate", "add_electricity_rate", 2)
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(id,home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,$2,1,'electricity',0.15,'kWh','USD','user_tariff','customer',$3,false,$4)`, tariff, home, now, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("active", "", 3)
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status) VALUES($1,$2,1,'computer.hours_per_week',1,2,'unknown')`, home, device); err != nil {
		t.Fatal(err)
	}
	check("candidate", "hours_per_week", 1)
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2 WHERE id=$1`, device); err != nil {
		t.Fatal(err)
	}
	check("candidate", "hours_per_week", 1)
}

func TestGasDryerRecommendationReadsExactUsageAndFreshThermRate(t *testing.T) {
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
	owner, home, device, fact, answer, rate := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	hash := strings.Repeat("b", 64)
	identity := `{"brand":"Acme","full_model":"G123","variant":"US-GAS","fuel":"natural_gas","match_quality":"confirmed_exact"}`
	statements := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Gas dryer','dryer',$3)`, []any{device, home, identity}},
		{`INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'test',1)`, []any{hash}},
		{`INSERT INTO reviewed_usage_facts
		(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
		VALUES($1,$2,'dryer','Acme','G123','natural_gas','US-GAS','confirmed_exact','G123','G123','cycle_energy',0.25,'therm/cycle','normal cycle','normal cycle','{"status":"documented","components":["appliance_gas"]}','oem_pdf','https://example.org/gas-dryer.pdf',$2,0,$2,'reviewer',$3,'gas-dryer.json','{}')`, []any{fact, hash, now}},
	}
	for _, stmt := range statements {
		if _, err = pool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	read := func() Action {
		t.Helper()
		items, readErr := store.Current(ctx, owner, home, now)
		if readErr != nil || len(items) != 1 {
			t.Fatalf("gas evidence: %+v %v", items, readErr)
		}
		actions := EvaluateGasEnabled(items[0])
		if len(actions) != 1 {
			t.Fatalf("gas actions: %+v", actions)
		}
		return actions[0]
	}
	if action := read(); action.NextQuestion != "cycles_per_week" || len(action.EvidenceRefs) != 1 {
		t.Fatalf("source: %+v", action)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(id,home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at)
	 VALUES($1,$2,$3,1,'dryer.cycles_per_week',1,1,'confirmed','3',$4)`, answer, home, device, now); err != nil {
		t.Fatal(err)
	}
	if action := read(); action.NextQuestion != "add_gas_rate" || len(action.EvidenceRefs) != 2 {
		t.Fatalf("usage: %+v", action)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
	 VALUES($1,1,'natural_gas',1.10,'therm','USD','user_tariff','old gas bill',$2,false,$3)`, home, now.AddDate(-2, 0, 0), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if action := read(); action.State != "candidate" || action.NextQuestion != "add_gas_rate" {
		t.Fatalf("stale rate: %+v", action)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(id,home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
	 VALUES($1,$2,2,'natural_gas',1.20,'therm','USD','user_tariff','customer',$3,false,$4)`, rate, home, now, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if action := read(); action.State != "active" || len(action.EvidenceRefs) != 3 || action.SavingsAmount != nil {
		t.Fatalf("active: %+v", action)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status)
	 VALUES($1,$2,1,'dryer.cycles_per_week',1,2,'unknown')`, home, device); err != nil {
		t.Fatal(err)
	}
	if action := read(); action.State != "candidate" || action.NextQuestion != "cycles_per_week" {
		t.Fatalf("unknown cycles: %+v", action)
	}
}
