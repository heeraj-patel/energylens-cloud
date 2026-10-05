package usagefactapi

import (
	"context"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActiveExactFactsOnly(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "usage_api_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	hash := strings.Repeat("a", 64)
	review := strings.Repeat("b", 64)
	source := strings.Repeat("c", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'test',3)`, hash); err != nil {
		t.Fatal(err)
	}
	exact := uuid.NewString()
	family := uuid.NewString()
	hourly := uuid.NewString()
	insert := func(id, quality, scope, kind, value string) {
		t.Helper()
		unit := "kWh/cycle"
		var cycle any = "Normal"
		var mode any = nil
		if kind == "hourly_energy" {
			unit = "kWh/hour"
			cycle = nil
			mode = "Active"
		}
		_, e := pool.Exec(ctx, `INSERT INTO reviewed_usage_facts(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
  VALUES($1,$2,'washer','Acme','W123','electricity','US-120V',$3,$4,$4,$5,$6::numeric,$7,'Official test',$8,$9,'{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.test/manual',$10,0,$11,'reviewer',now(),'test.json','{}')`, id, hash, quality, scope, kind, value, unit, cycle, mode, source, review)
		if e != nil {
			t.Fatal(e)
		}
	}
	insert(exact, "confirmed_exact", "W123", "cycle_energy", "0.42")
	insert(family, "documented_variant_family", "W12*", "cycle_energy", "0.9")
	insert(hourly, "confirmed_exact", "W123", "hourly_energy", "0.12")
	store := PGStore{Pool: pool}
	scope := Scope{"washer", "Acme", "W123", "electricity", "US-120V"}
	facts, err := store.Find(ctx, scope)
	if err != nil || len(facts) != 2 {
		t.Fatalf("facts %d: %v", len(facts), err)
	}
	for _, f := range facts {
		if f.ID == family || f.SourceSHA256 != source || f.PublicationSHA256 != hash || f.ReviewSHA256 != review {
			t.Fatalf("bad fact: %+v", f)
		}
	}
	scope.Model = "W12"
	facts, err = store.Find(ctx, scope)
	if err != nil || len(facts) != 0 {
		t.Fatalf("partial match: %d %v", len(facts), err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_fact_revisions(id,publication_sha256,prior_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,'retract','wrong scope','reviewer',now(),$4)`, uuid.NewString(), hash, exact, review); err != nil {
		t.Fatal(err)
	}
	scope.Model = "W123"
	facts, err = store.Find(ctx, scope)
	if err != nil || len(facts) != 1 || facts[0].ID != hourly {
		t.Fatalf("after retraction: %+v %v", facts, err)
	}
}
