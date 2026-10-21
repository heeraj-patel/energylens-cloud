package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedBareProduct(t *testing.T, pool *pgxpool.Pool, fuel, model, quality string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Acme',$2,$3,$4,'{}')`, id, model, fuel, quality); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFindLinkGasProductsLinkWithoutFactsElectricDoNot(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_gas_nofact_")
	ctx := context.Background()
	gasExact := seedBareProduct(t, pool, "natural_gas", "GD100A", "confirmed_exact")
	gasFamily := seedBareProduct(t, pool, "propane", "GF2000*", "documented_variant_family")
	gasTrim := seedBareProduct(t, pool, "fuel_oil", "GT3000ABCD", "confirmed_exact")
	seedBareProduct(t, pool, "electricity", "ED100A", "confirmed_exact")
	seedBareProduct(t, pool, "electricity", "EF5000*", "documented_variant_family")
	seedBareProduct(t, pool, "electricity", "ET6000ABCD", "confirmed_exact")
	seedBareProduct(t, pool, "natural_gas", "GM7000ABCD", "confirmed_exact")
	seedBareProduct(t, pool, "natural_gas", "GM7000ABCE", "confirmed_exact")
	for _, c := range []struct {
		name, model, fuel, want, quality string
	}{
		{"gas exact", "GD100A", "natural_gas", gasExact, "confirmed_exact"},
		{"gas family", "GF2000B", "propane", gasFamily, "documented_variant_family"},
		{"gas prefix trim", "GT3000ABCX", "fuel_oil", gasTrim, "documented_variant_family"},
		{"gas exact of another fuel", "GD100A", "propane", "", ""},
		{"several fact-less gas trims never agree", "GM7000ABCX", "natural_gas", "", ""},
		{"electric exact without a fact", "ED100A", "electricity", "", ""},
		{"electric family without a fact", "EF5000B", "electricity", "", ""},
		{"electric trim without a fact", "ET6000ABCX", "electricity", "", ""},
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

func TestSearchStillOmitsFactlessFamilyProducts(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_gas_search_")
	seedBareProduct(t, pool, "natural_gas", "GF2000*", "documented_variant_family")
	found, err := (PGStore{Pool: pool}).Search(context.Background(), Search{Brand: "Acme", Model: "GF2000B", Fuel: "natural_gas"})
	if err != nil || len(found) != 0 {
		t.Fatalf("search listed a fact-less family product: %d %v", len(found), err)
	}
}
