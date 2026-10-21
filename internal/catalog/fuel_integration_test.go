package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
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
