package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedWattsProduct(t *testing.T, pool *pgxpool.Pool, brand, model string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'fan',$2,$3,'electricity','confirmed_exact',jsonb_build_object('brand',$2::text,'brand_key',$4::text,'full_model',$3::text))`,
		id, brand, model, BrandKey(brand)); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedRatedInput(t *testing.T, pool *pgxpool.Pool, brand, model string, retract bool) {
	t.Helper()
	ctx := context.Background()
	pub, fact := strings.Repeat("d", 64), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO reviewed_rated_input_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'test',1) ON CONFLICT DO NOTHING`, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reviewed_rated_input_facts(id,publication_sha256,category,manufacturer,model,source_kind,matched_model,rated_input_watts,unit,source_url,manufacturer_source_url,source_sha256,review_sha256,reviewer,reviewed_at,usage_basis,source_manifest,fact_json)
	VALUES($1,$2,'fan',$3,$4,'manual_research',$4,60,'W','https://example.org','https://example.org',$2,$2,'t',now(),'b','m','{}')`, fact, pub, brand, model); err != nil {
		t.Fatal(err)
	}
	if retract {
		if _, err := pool.Exec(ctx, `INSERT INTO reviewed_rated_input_revisions(id,publication_sha256,prior_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,'retract','r','t',now(),$2)`, uuid.NewString(), pub, fact); err != nil {
			t.Fatal(err)
		}
	}
}

func seedStandby(t *testing.T, pool *pgxpool.Pool, brand, model string) {
	t.Helper()
	ctx := context.Background()
	pub := strings.Repeat("c", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'test',1) ON CONFLICT DO NOTHING`, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reviewed_usage_facts(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,operation_mode,included_energy,source_kind,source_url,source_sha256,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
	VALUES($1,$2,'fan',$3,$4,'electricity','','confirmed_exact','model',$4,'standby_power',2,'W','b','standby','{"status":"documented","components":["standby_electricity"]}','manual_research','https://example.org',$2,$2,'t',now(),'m','{}')`, uuid.NewString(), pub, brand, model); err != nil {
		t.Fatal(err)
	}
}

func TestFindLinkWattsAndStandbyOnly(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_watts_")
	ids := seedTrim(t, pool,
		trimSeed{brand: "Acme", model: "DUP-100", quality: "confirmed_exact", source: "epa_exact", kwh: 100},
	)
	rated := seedWattsProduct(t, pool, "Acme", "RATED-1")
	standby := seedWattsProduct(t, pool, "Acme", "STANDBY-1")
	seedWattsProduct(t, pool, "Acme", "IDENTITY-1")
	seedWattsProduct(t, pool, "Acme", "RETRACTED-1")
	seedWattsProduct(t, pool, "Acme", "DUP-100 ")
	seedRatedInput(t, pool, "Acme", "RATED-1", false)
	seedStandby(t, pool, "ACME", "standby 1")
	seedRatedInput(t, pool, "Acme", "RETRACTED-1", true)
	seedRatedInput(t, pool, "Acme", "DUP-100 ", false)
	cases := []struct{ name, model, want string }{
		{"rated input only", "RATED-1", rated},
		{"standby only", "STANDBY-1", standby},
		{"identity only", "IDENTITY-1", ""},
		{"retracted rated input", "RETRACTED-1", ""},
		{"annual preferred over watts-only duplicate", "DUP-100", ids["DUP-100"]},
	}
	for _, c := range cases {
		link, found, err := FindLink(context.Background(), pool, Search{Brand: "Acme", Model: c.model, Fuel: "electricity", Category: "fan"})
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if found {
				t.Errorf("%s: unexpected link %+v", c.name, link)
			}
			continue
		}
		if !found || link.ProductID != c.want || link.MatchQuality != "confirmed_exact" {
			t.Errorf("%s: want %s, got %+v found=%v", c.name, c.want, link, found)
		}
	}
}
