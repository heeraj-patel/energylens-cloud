package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogProductsKeepGasFuelAndNeverLinkAsElectric(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_fuel_")
	ctx := context.Background()
	gas := map[string]string{}
	for fuel, model := range map[string]string{"natural_gas": "GD100A", "propane": "GD200B", "fuel_oil": "GD300C"} {
		id := uuid.NewString()
		gas[fuel] = id
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Acme',$2,$3,'confirmed_exact','{}')`, id, model, fuel); err != nil {
			t.Fatalf("%s product: %v", fuel, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Acme','XX','coal','confirmed_exact','{}')`, uuid.NewString()); err == nil {
		t.Fatal("unknown fuel accepted")
	}
	store := PGStore{Pool: pool}
	if found, err := store.Search(ctx, Search{Brand: "Acme", Model: "GD100A", Fuel: "electricity"}); err != nil || len(found) != 0 {
		t.Fatalf("electric search returned a gas product: %d %v", len(found), err)
	}
	if found, err := store.Search(ctx, Search{Brand: "Acme", Model: "GD100A", Fuel: "natural_gas"}); err != nil || len(found) != 1 {
		t.Fatalf("gas search: %d %v", len(found), err)
	}
	if _, linked, err := FindLink(ctx, pool, Search{Brand: "Acme", Model: "GD100A", Fuel: "electricity", Category: "dryer"}); err != nil || linked {
		t.Fatalf("electric identity linked to a gas product: %v %v", linked, err)
	}
}

func seedFuelProduct(t *testing.T, pool *pgxpool.Pool, fuel, brand, model, quality, source string) string {
	t.Helper()
	ctx := context.Background()
	hash := strings.Repeat("f", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0) ON CONFLICT DO NOTHING`, hash); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	product, _ := json.Marshal(map[string]any{"brand": brand, "brand_key": BrandKey(brand), "full_model": model})
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer',$2,$3,$4,$5,$6)`, id, brand, model, fuel, quality, product); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
	VALUES($1,$2,$3,'annual_energy',100,'kWh',jsonb_build_object('matched_model',$4::text),'{}',$5,'https://example.org/f',$3,'source_row','m',now(),1,'{}')`, uuid.NewString(), id, hash, model, source); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFindLinkMatchesOnlyProductsOfTheSameFuel(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_fuel_link_")
	ctx := context.Background()
	gasExact := seedFuelProduct(t, pool, "natural_gas", "Acme", "GD100A", "confirmed_exact", "epa_exact")
	elecSame := seedFuelProduct(t, pool, "electricity", "Acme", "GD100A", "confirmed_exact", "epa_exact")
	gasFamily := seedFuelProduct(t, pool, "natural_gas", "Acme", "GF2000*", "documented_variant_family", "epa_family")
	gasTrim := seedFuelProduct(t, pool, "natural_gas", "Acme", "GT3000ABCD", "confirmed_exact", "epa_exact")
	seedFuelProduct(t, pool, "natural_gas", "Acme", "GO400A", "confirmed_exact", "epa_exact")
	elecFamily := seedFuelProduct(t, pool, "electricity", "Acme", "EF5000*", "documented_variant_family", "epa_family")
	elecTrim := seedFuelProduct(t, pool, "electricity", "Acme", "ET6000ABCD", "confirmed_exact", "epa_exact")
	for _, c := range []struct {
		name, model, fuel, want, quality string
	}{
		{"gas exact", "GD100A", "natural_gas", gasExact, "confirmed_exact"},
		{"electric exact beside a gas product", "GD100A", "electricity", elecSame, "confirmed_exact"},
		{"propane never takes the gas product", "GD100A", "propane", "", ""},
		{"gas family", "GF2000B", "natural_gas", gasFamily, "documented_variant_family"},
		{"electric never takes a gas family", "GF2000B", "electricity", "", ""},
		{"gas prefix trim", "GT3000ABCX", "natural_gas", gasTrim, "documented_variant_family"},
		{"electric never takes a gas trim", "GT3000ABCX", "electricity", "", ""},
		{"electric never takes a gas-only exact", "GO400A", "electricity", "", ""},
		{"gas never takes an electric family", "EF5000B", "natural_gas", "", ""},
		{"gas never takes an electric trim", "ET6000ABCX", "natural_gas", "", ""},
		{"electric family still links", "EF5000B", "electricity", elecFamily, "documented_variant_family"},
		{"electric trim still links", "ET6000ABCX", "electricity", elecTrim, "documented_variant_family"},
	} {
		link, found, err := FindLink(ctx, pool, Search{Brand: "Acme", Model: c.model, Fuel: c.fuel, Category: "dryer"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == "" && found || c.want != "" && (!found || link.ProductID != c.want || link.MatchQuality != c.quality) {
			t.Errorf("%s: got %+v found=%v, want %s %s", c.name, link, found, c.want, c.quality)
		}
	}
}
