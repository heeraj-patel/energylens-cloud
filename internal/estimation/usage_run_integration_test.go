package estimation

import (
	"context"
	"encoding/json"
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

func TestReviewedUsageSavedAnswerEstimate(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "usage_estimate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, appliance, fact := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	identity := `{"brand":"Acme","full_model":"W123","variant":"US-120V","fuel":"electricity","match_quality":"confirmed_exact"}`
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','America/Chicago')`, home, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Washer','washer',$3)`, appliance, home, identity); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store := PGRunStore{Pool: pool}
	read := func() estimateBody {
		t.Helper()
		runs, e := store.ListRuns(ctx, owner, home, &appliance, now)
		if e != nil || len(runs) != 1 {
			t.Fatalf("estimate: %d %v", len(runs), e)
		}
		return decodeEstimate(t, runs[0])
	}
	initial := read()
	if initial.Kind != "unavailable" {
		t.Fatalf("missing source: %+v", initial)
	}
	hash := strings.Repeat("a", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'operator',1)`, hash); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_facts
		(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
		VALUES($1,$2,'washer','Acme','W123','electricity','US-120V','confirmed_exact','W123','W123','cycle_energy',0.5,'kWh/cycle','normal cycle','normal cycle','{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.org/manual.pdf',$2,0,$2,'reviewer',now(),'washer.json','{}')`, fact, hash)
	if err != nil {
		t.Fatal(err)
	}
	withoutAnswer := read()
	if withoutAnswer.Kind != "unavailable" || withoutAnswer.UnavailableReason != "rate_unavailable" || withoutAnswer.ID == initial.ID {
		t.Fatalf("missing rate: %+v", withoutAnswer)
	}
	var sourceInvalidations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM estimate_run_invalidations WHERE run_id=$1`, initial.ID).Scan(&sourceInvalidations); err != nil || sourceInvalidations != 1 {
		t.Fatalf("source arrival did not invalidate prior run: %d %v", sourceInvalidations, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','bill',$2,false,$3)`, home, now, now.Add(-35*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	withoutAnswer = read()
	if withoutAnswer.Kind != "unavailable" || withoutAnswer.UnavailableReason != "usage_unknown" {
		t.Fatalf("unknown usage: %+v", withoutAnswer)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'washer.cycles_per_week',1,1,'confirmed','0',now())`, home, appliance); err != nil {
		t.Fatal(err)
	}
	zero := read()
	if zero.Kind != "estimated_useful_cost" || zero.Cost == nil || zero.Cost.Value != "0.000000" || zero.ID == withoutAnswer.ID {
		t.Fatalf("confirmed zero: %+v", zero)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'washer.cycles_per_week',1,2,'confirmed','7',now())`, home, appliance); err != nil {
		t.Fatal(err)
	}
	used := read()
	if used.Kind != "estimated_useful_cost" || used.Cost == nil || used.Cost.Value == "0.000000" || used.ID == zero.ID {
		t.Fatalf("confirmed usage: %+v", used)
	}
	if got := read(); got.ID != used.ID {
		t.Fatalf("same dependencies created new run: %+v", got)
	}
	var snapshot []byte
	if err = pool.QueryRow(ctx, `SELECT dependency_snapshot FROM estimate_runs WHERE id=$1`, used.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var deps map[string]any
	if err = json.Unmarshal(snapshot, &deps); err != nil {
		t.Fatal(err)
	}
	if deps["reviewed_usage_fact_id"] != fact || deps["confirmed_usage_revision"] != float64(2) || deps["projected_usage"] == "" {
		t.Fatalf("usage dependency snapshot: %v", deps)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2 WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	stale := read()
	if stale.Kind != "unavailable" || stale.UnavailableReason != "usage_unknown" {
		t.Fatalf("old identity revision reused answer: %+v", stale)
	}
	var invalidations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM estimate_run_invalidations WHERE run_id=$1`, used.ID).Scan(&invalidations); err != nil || invalidations != 1 {
		t.Fatalf("missing invalidation: %d %v", invalidations, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status) VALUES($1,$2,2,'washer.cycles_per_week',1,3,'unknown')`, home, appliance); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Kind != "unavailable" || got.UnavailableReason != "usage_unknown" {
		t.Fatalf("unknown became zero: %+v", got)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,2,'washer.cycles_per_week',1,4,'confirmed','"14"',now())`, home, appliance); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Kind != "unavailable" || got.UnavailableReason != "usage_unknown" {
		t.Fatalf("quoted weekly value accepted: %+v", got)
	}
	if _, err = pool.Exec(ctx, `UPDATE reviewed_usage_facts SET value=9 WHERE id=$1`, fact); err == nil {
		t.Fatal("immutable source changed")
	}
	computer := uuid.NewString()
	computerFact := uuid.NewString()
	computerIdentity := `{"brand":"Acme","full_model":"C456","variant":"US-120V","fuel":"electricity","match_quality":"confirmed_exact"}`
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Computer','computer',$3)`, computer, home, computerIdentity); err != nil {
		t.Fatal(err)
	}
	otherHash := strings.Repeat("b", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'operator',1)`, otherHash); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_facts
		(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
		VALUES($1,$2,'computer','Acme','C456','electricity','US-120V','confirmed_exact','C456','C456','hourly_energy',0.25,'kWh/hour','active use','active use','{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.org/computer.pdf',$2,0,$2,'reviewer',now(),'computer.json','{}')`, computerFact, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'computer.hours_per_week',1,1,'confirmed','14',now())`, home, computer); err != nil {
		t.Fatal(err)
	}
	computerRuns, err := store.ListRuns(ctx, owner, home, &computer, now)
	if err != nil || len(computerRuns) != 1 {
		t.Fatalf("hourly estimate: %d %v", len(computerRuns), err)
	}
	computerResult := decodeEstimate(t, computerRuns[0])
	if computerResult.Kind != "estimated_useful_cost" || computerResult.Cost == nil || computerResult.Cost.Value == "0.000000" {
		t.Fatalf("hourly useful cost: %+v", computerResult)
	}
	familyAppliance := uuid.NewString()
	familyFact := uuid.NewString()
	familyIdentity := `{"brand":"Acme","full_model":"C789","variant":"US-120V","fuel":"electricity","match_quality":"confirmed_exact"}`
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Other computer','computer',$3)`, familyAppliance, home, familyIdentity); err != nil {
		t.Fatal(err)
	}
	familyHash := strings.Repeat("c", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'operator',1)`, familyHash); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_facts
		(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
		VALUES($1,$2,'computer','Acme','C789','electricity','US-120V','documented_variant_family','C78*','C78*','hourly_energy',0.25,'kWh/hour','active use','active use','{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.org/family.pdf',$2,0,$2,'reviewer',now(),'family.json','{}')`, familyFact, familyHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'computer.hours_per_week',1,1,'confirmed','14',now())`, home, familyAppliance); err != nil {
		t.Fatal(err)
	}
	familyRuns, err := store.ListRuns(ctx, owner, home, &familyAppliance, now)
	if err != nil || len(familyRuns) != 1 {
		t.Fatalf("family estimate: %d %v", len(familyRuns), err)
	}
	familyResult := decodeEstimate(t, familyRuns[0])
	if familyResult.Kind != "unavailable" || familyResult.UnavailableReason != "basis_incomparable" {
		t.Fatalf("family scope silently selected: %+v", familyResult)
	}
	annualPublication := strings.Repeat("d", 64)
	annualProduct := uuid.NewString()
	annualFact := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'operator',1,1)`, annualPublication); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,variant,fuel,match_quality,product_json) VALUES($1,'computer','Acme','C789','US-120V','electricity','confirmed_exact','{}')`, annualProduct); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',365,'kWh','{}','{}','epa_exact','https://example.org/annual',$3,'reviewed_row_manifest','annual.json',now(),1,'{}')`, annualFact, annualProduct, annualPublication); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET catalog_product_id=$2,version=2 WHERE id=$1`, familyAppliance, annualProduct); err != nil {
		t.Fatal(err)
	}
	annualRuns, err := store.ListRuns(ctx, owner, home, &familyAppliance, now)
	if err != nil || len(annualRuns) != 1 {
		t.Fatalf("annual fallback: %d %v", len(annualRuns), err)
	}
	annualResult := decodeEstimate(t, annualRuns[0])
	if annualResult.Kind != "estimated_operating_cost" || annualResult.Cost == nil {
		t.Fatalf("family usage suppressed annual estimate: %+v", annualResult)
	}
}
