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
