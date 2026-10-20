package catalog

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

func TestFamilySearchAndLinkAtBulkScale(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "catalog_scale_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	cfg.MaxConns = 1
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
	hash := strings.Repeat("c", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0)`, hash); err != nil {
		t.Fatal(err)
	}
	// 1100 same-length decoys sort before the one product whose pattern matches.
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json)
	SELECT ('00000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,'refrigerator','LG','DC'||lpad(i::text,6,'0'),'electricity','documented_variant_family','{"brand_key":"lg"}'::jsonb
	FROM generate_series(1,1100) i`); err != nil {
		t.Fatal(err)
	}
	target := "ffffffff-0000-0000-0000-000000000001"
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'refrigerator','LG','TARGET01','electricity','documented_variant_family','{"brand_key":"lg","full_model":"TARGET01"}')`, target); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
	SELECT md5('f'||p.id::text)::uuid, p.id,$1,'annual_energy',300,'kWh',jsonb_build_object('matched_model',CASE WHEN p.id::text LIKE 'ffffffff%' THEN 'TARGET0*' ELSE 'NOMATCH*' END),'{}','epa_family','https://example.org/f',$1,'source_row','m',now(),1,'{}'
	FROM catalog_products p`, hash); err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	results, err := store.Search(ctx, Search{Brand: "LG Electronics", Model: "TARGET02", Fuel: "electricity"})
	if err != nil || len(results) != 1 || !strings.Contains(string(results[0]), "\"full_model\":\"TARGET01\"") {
		t.Fatalf("family search past 1000 rows: %d %v", len(results), err)
	}
	link, found, err := FindLink(ctx, pool, Search{Brand: "LG Electronics", Model: "TARGET02", Fuel: "electricity"})
	if err != nil || !found || link.ProductID != target || link.MatchQuality != "documented_variant_family" {
		t.Fatalf("family link past 1000 rows: %+v %v %v", link, found, err)
	}
}
