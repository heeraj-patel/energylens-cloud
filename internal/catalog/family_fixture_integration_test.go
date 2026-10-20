package catalog

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migratedCatalogPool returns a single-connection pool on a fresh migrated schema.
func migratedCatalogPool(t *testing.T, prefix string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
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

// Family products are stored as the datawiz loaders write them: full_model and
// test_basis.matched_model both hold the published wildcard pattern.
func TestFamilyLinkWithLoadedPatterns(t *testing.T) {
	var fixture struct {
		Products []struct {
			Category, Brand, Pattern string
			SourceKind               string  `json:"source_kind"`
			AnnualKWh                float64 `json:"annual_kwh"`
		}
		Appliances []struct{ Brand, Model, Want string }
	}
	raw, err := os.ReadFile("testdata/family_patterns.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	pool := migratedCatalogPool(t, "catalog_family_")
	ctx := context.Background()
	hash := strings.Repeat("d", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0)`, hash); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, p := range fixture.Products {
		id := uuid.NewString()
		ids[p.Pattern] = id
		product, _ := json.Marshal(map[string]any{"brand": p.Brand, "brand_key": BrandKey(p.Brand), "full_model": p.Pattern})
		if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,$2,$3,$4,'electricity','documented_variant_family',$5)`, id, p.Category, p.Brand, p.Pattern, product); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
		VALUES($1,$2,$3,'annual_energy',$4,'kWh',jsonb_build_object('matched_model',$5::text),'{}',$6,'https://example.org/f',$3,'source_row','m',now(),1,'{}')`, uuid.NewString(), id, hash, p.AnnualKWh, p.Pattern, p.SourceKind); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range fixture.Appliances {
		link, found, err := FindLink(ctx, pool, Search{Brand: a.Brand, Model: a.Model, Fuel: "electricity"})
		if err != nil {
			t.Fatal(err)
		}
		if a.Want == "" {
			if found {
				t.Errorf("%s %s: unexpected link %+v", a.Brand, a.Model, link)
			}
			continue
		}
		if !found || link.ProductID != ids[a.Want] || link.MatchQuality != "documented_variant_family" {
			t.Errorf("%s %s: want family %s, got %+v found=%v", a.Brand, a.Model, a.Want, link, found)
		}
	}
}
