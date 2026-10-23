package recommendation

import (
	"context"
	tariffpkg "energylens.local/cloud/internal/tariff"
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
	// Match PostgreSQL's timestamp precision at the exact expiry boundary.
	now := time.Now().UTC().Truncate(time.Microsecond)
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
	// A lifecycle event records user intent, not lasting evidence. The same
	// recommendation stays supported just before the tariff age limit, then
	// expires at the exact boundary without a tariff mutation.
	event := LifecycleEvent{ApplianceID: device, RuleID: "computer.sleep_settings", RuleVersion: 1, State: "accepted"}
	if _, err = store.Record(ctx, owner, home, uuid.NewString(), event); err != nil {
		t.Fatal(err)
	}
	readLifecycle := func(at time.Time, wantState string, wantTariff bool) {
		t.Helper()
		items, readErr := store.Current(ctx, owner, home, at)
		if readErr != nil || len(items) != 1 {
			t.Fatalf("current recommendation at %s: %+v %v", at, items, readErr)
		}
		gotTariff := items[0].Tariff != nil
		if gotTariff != wantTariff {
			t.Fatalf("tariff presence at %s = %t, want %t", at, gotTariff, wantTariff)
		}
		actions := Evaluate(items[0])
		latest, latestErr := store.Latest(ctx, owner, home)
		if latestErr != nil {
			t.Fatal(latestErr)
		}
		visible := ApplyLifecycle(actions, latest, at)
		if len(visible) != 1 || visible[0].State != wantState {
			t.Fatalf("lifecycle state at %s = %+v, want %q", at, visible, wantState)
		}
		for _, ref := range visible[0].EvidenceRefs {
			if ref.Kind == "tariff" && !wantTariff {
				t.Fatalf("expired tariff still cited at %s: %+v", at, visible[0])
			}
		}
	}
	readLifecycle(now.Add(tariffpkg.ElectricityMaxAge-time.Nanosecond), "accepted", true)
	readLifecycle(now.Add(tariffpkg.ElectricityMaxAge), "expired", false)
	var retained int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_events WHERE home_id=$1`, home).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("historical recommendation event count = %d, %v", retained, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status) VALUES($1,$2,1,'computer.hours_per_week',1,2,'unknown')`, home, device); err != nil {
		t.Fatal(err)
	}
	check("candidate", "hours_per_week", 1)
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2 WHERE id=$1`, device); err != nil {
		t.Fatal(err)
	}
	check("candidate", "hours_per_week", 1)
	replacement := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',40,'kWh','{}','{}','epa_exact','https://example.org/correction',$4,'reviewed_row_manifest','sources/correction.json',$5,2,'{}')`, replacement, product, hash, strings.Repeat("b", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_changes(id,publication_sha256,prior_fact_id,replacement_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,$4,'supersede','correct annual figure','reviewer',$5,$6)`, uuid.NewString(), hash, fact, replacement, now.Add(time.Second), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	items, err := store.Current(ctx, owner, home, now)
	if err != nil || len(items) != 1 || len(items[0].Facts) != 1 || items[0].Facts[0].ID != replacement {
		t.Fatalf("recommendation retained superseded fact: %+v %v", items, err)
	}
	check("candidate", "hours_per_week", 1)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_changes(id,publication_sha256,prior_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,'retract','withdraw corrected figure','reviewer',$4,$5)`, uuid.NewString(), hash, replacement, now.Add(2*time.Second), strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	check("candidate", "find_reviewed_energy_source", 0)
	light := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"fuel":"electricity","match_quality":"unresolved"}')`, light, home); err != nil {
		t.Fatal(err)
	}
	items, err = store.Current(ctx, owner, home, now)
	if err != nil || len(items) != 2 || items[1].categoryKey != "light" {
		t.Fatalf("category registry lookup: %+v %v", items, err)
	}
	if got := Evaluate(items[1]); len(got) != 1 || got[0].RuleID != "lighting.equivalent_bulb" {
		t.Fatalf("mapped lighting rule: %+v", got)
	}
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
	 VALUES($1,$2,$3,1,'dryer.cycles_per_week',2,1,'confirmed','3',$4)`, answer, home, device, now); err != nil {
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

func TestCurrentReadsNonIntegerV2AnswerOptions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_frac_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, dryer, computer := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dryer','dryer','{"fuel":"electricity"}')`, []any{dryer, home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'PC','computer','{"fuel":"electricity"}')`, []any{computer, home}},
		{`INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'dryer.cycles_per_week',2,1,'confirmed','1.5',$3)`, []any{home, dryer, now}},
		{`INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'computer.hours_per_week',2,1,'confirmed','45.5',$3)`, []any{home, computer, now}},
	} {
		if _, err = pool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	items, err := (PGStore{Pool: pool}).Current(ctx, owner, home, now)
	if err != nil || len(items) != 2 {
		t.Fatalf("current: %+v %v", items, err)
	}
	for _, item := range items {
		switch item.Category {
		case "dryer":
			if item.CyclesPerWeek == nil || *item.CyclesPerWeek != 1.5 || item.Answers["cycles_per_week"].Kind != "confirmed_answer" {
				t.Fatalf("dryer cycles: %+v", item)
			}
		case "computer":
			if item.HoursPerWeek == nil || *item.HoursPerWeek != 45.5 || item.Answers["hours_per_week"].Kind != "confirmed_answer" {
				t.Fatalf("computer hours: %+v", item)
			}
		}
	}
}

func TestWeatherTipsAreReadSeparatelyFromLegacyResearch(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_weather_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	run := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,'efficiency','{}','realtime','complete')`, run); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"habits", "weather_hot", "weather_cold"} {
		observation := uuid.NewString()
		value := `{"context":"` + c + `","applicable_categories":["home"]}`
		if _, err = pool.Exec(ctx, `INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at) VALUES($1,$2,'efficiency',$3,'efficiency_tip',$4,$5,'{"adapter":"fixture_official","url":"https://example.org/tip","retrieved_at":"2026-10-12T00:00:00Z"}','approved','2026-10-12T00:00:00Z')`, observation, run, "efficiency:"+c, c, value); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES('efficiency',$1,'efficiency_tip',$2,$3)`, "efficiency:"+c, c, observation); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	legacy, err := store.Research(ctx)
	if err != nil || len(legacy) != 1 || legacy[0].RecordKey != "habits" {
		t.Fatalf("legacy research: %+v, %v", legacy, err)
	}
	weatherTips, err := store.WeatherTips(ctx)
	if err != nil || len(weatherTips) != 2 || weatherTips[0].RecordKey != "weather_hot" || weatherTips[1].RecordKey != "weather_cold" {
		t.Fatalf("weather tips: %+v, %v", weatherTips, err)
	}
}
