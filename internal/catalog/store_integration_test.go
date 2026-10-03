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

func TestReviewedCatalogExactSearchAndFacts(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "catalog_lookup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',2,1)`, hash); err != nil {
		t.Fatal(err)
	}
	neutral, regional := uuid.NewString(), uuid.NewString()
	for _, item := range []struct{ id, region string }{{neutral, ""}, {regional, "US"}} {
		var region any
		if item.region != "" {
			region = item.region
		}
		product, _ := json.Marshal(map[string]any{"id": item.id, "category": "computer", "brand": "Acme", "full_model": "A123", "variant": nil, "region": region, "fuel": "electricity", "match_quality": "confirmed_exact"})
		if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,region,fuel,match_quality,product_json) VALUES($1,'computer','Acme','A123',$2,'electricity','confirmed_exact',$3)`, item.id, region, product); err != nil {
			t.Fatal(err)
		}
	}
	fact := map[string]any{"id": uuid.NewString(), "kind": "annual_energy", "value": "20", "unit": "kWh", "test_basis": map[string]any{}, "included_energy": map[string]any{"status": "not_specified_by_source"}, "source_kind": "epa_exact", "source_url": "https://example.org/fact", "source_hash": hash, "source_hash_basis": "reviewed_row_manifest", "source_manifest": "sources/example.json", "reviewed_at": "2026-09-30T00:00:00Z", "revision": 1}
	factJSON, _ := json.Marshal(fact)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',20,'kWh','{}','{}','epa_exact','https://example.org/fact',$3,'reviewed_row_manifest','sources/example.json','2026-09-30T00:00:00Z',1,$4)`, fact["id"], neutral, hash, factJSON); err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	results, err := store.Search(ctx, Search{Brand: "acme", Model: "a123", Fuel: "electricity"})
	if err != nil || len(results) != 1 {
		t.Fatalf("neutral search: %d %v", len(results), err)
	}
	var product struct {
		ID    string           `json:"id"`
		Facts []map[string]any `json:"facts"`
	}
	if err = json.Unmarshal(results[0], &product); err != nil {
		t.Fatal(err)
	}
	if product.ID != neutral || len(product.Facts) != 1 || product.Facts[0]["source_fetched_at"] != nil {
		t.Fatalf("product evidence: %+v", product)
	}
	us := "US"
	results, err = store.Search(ctx, Search{Brand: "ACME", Model: "A123", Region: &us, Fuel: "electricity"})
	if err != nil || len(results) != 2 {
		t.Fatalf("regional search: %d %v", len(results), err)
	}
	if !strings.Contains(string(results[0]), regional) {
		t.Fatalf("regional result not first: %s", results[0])
	}
	other := "CA"
	results, err = store.Search(ctx, Search{Brand: "Acme", Model: "A123", Region: &other, Fuel: "electricity"})
	if err != nil || len(results) != 1 {
		t.Fatalf("foreign region leaked: %d %v", len(results), err)
	}
	results, err = store.Search(ctx, Search{Brand: "Acme", Model: "A12", Fuel: "electricity"})
	if err != nil || len(results) != 0 {
		t.Fatalf("partial match: %d %v", len(results), err)
	}
	// The stored reviewed product is a representative model. The only scope
	// eligible for family search is the literal pattern in its reviewed fact.
	familyID := uuid.NewString()
	familyProduct, _ := json.Marshal(map[string]any{"id": familyID, "category": "washer", "brand": "LG", "full_model": "WM3070HWA", "variant": "US-120V", "region": "US", "fuel": "electricity", "match_quality": "documented_variant_family"})
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,variant,region,fuel,match_quality,product_json) VALUES($1,'washer','LG','WM3070HWA','US-120V','US','electricity','documented_variant_family',$2)`, familyID, familyProduct); err != nil {
		t.Fatal(err)
	}
	familyFactID := uuid.NewString()
	familyFact := map[string]any{"id": familyFactID, "kind": "annual_energy", "value": "312", "unit": "kWh", "test_basis": map[string]any{"matched_model": "WM3070H*A"}, "included_energy": map[string]any{"status": "not_specified_by_source"}, "source_kind": "oem_family", "source_url": "https://example.org/manual", "source_hash": hash, "source_hash_basis": "verified_source_document", "source_manifest": "sources/lg.json", "reviewed_at": "2026-09-30T00:00:00Z", "revision": 1}
	familyFactJSON, _ := json.Marshal(familyFact)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',312,'kWh',$4,'{}','oem_family','https://example.org/manual',$3,'verified_source_document','sources/lg.json','2026-09-30T00:00:00Z',1,$5)`, familyFactID, familyID, hash, `{"matched_model":"WM3070H*A"}`, familyFactJSON); err != nil {
		t.Fatal(err)
	}
	variant := "US-120V"
	results, err = store.Search(ctx, Search{Brand: "LG", Model: "WM3070HXA", Fuel: "electricity", Region: &us, Variant: &variant})
	if err != nil || len(results) != 1 {
		t.Fatalf("family candidate search: %d %v", len(results), err)
	}
	var candidate struct {
		ID           string            `json:"id"`
		FullModel    string            `json:"full_model"`
		QueryMatch   string            `json:"query_match"`
		MatchedModel string            `json:"matched_model"`
		VariantMatch string            `json:"variant_match"`
		Facts        []json.RawMessage `json:"facts"`
	}
	if err = json.Unmarshal(results[0], &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.ID != familyID || candidate.FullModel != "WM3070HWA" || candidate.QueryMatch != "documented_family" || candidate.MatchedModel != "WM3070HXA" || candidate.VariantMatch != "confirmed" || len(candidate.Facts) != 0 {
		t.Fatalf("candidate did not preserve representative identity without facts: %+v", candidate)
	}
	for _, q := range []Search{
		{Brand: "LG", Model: "WM3070HXB", Fuel: "electricity", Region: &us, Variant: &variant},
		{Brand: "LG", Model: "WM3070HXXA", Fuel: "electricity", Region: &us, Variant: &variant},
		{Brand: "LG", Model: "WM3070HXA", Fuel: "electricity", Region: &other, Variant: &variant},
		{Brand: "LG", Model: "WM3070HXA", Fuel: "natural_gas", Region: &us, Variant: &variant},
	} {
		results, err = store.Search(ctx, q)
		if err != nil || len(results) != 0 {
			t.Fatalf("mismatched family scope %+v: %d %v", q, len(results), err)
		}
	}
	results, err = store.Search(ctx, Search{Brand: "LG", Model: "WM3070HXA", Fuel: "electricity", Region: &us})
	if err != nil || len(results) != 1 {
		t.Fatalf("unresolved variant candidate: %d %v", len(results), err)
	}
	if err = json.Unmarshal(results[0], &candidate); err != nil || candidate.VariantMatch != "unresolved" || len(candidate.Facts) != 0 {
		t.Fatalf("unresolved variant exposed fact: %+v %v", candidate, err)
	}
	wrongVariant := "EU-230V"
	results, err = store.Search(ctx, Search{Brand: "LG", Model: "WM3070HXA", Fuel: "electricity", Region: &us, Variant: &wrongVariant})
	if err != nil || len(results) != 0 {
		t.Fatalf("wrong variant leaked: %d %v", len(results), err)
	}
	// A reviewed exact identity precedes a family candidate for the same query.
	exactID := uuid.NewString()
	exactProduct, _ := json.Marshal(map[string]any{"id": exactID, "category": "washer", "brand": "LG", "full_model": "WM3070HXA", "variant": "US-120V", "region": "US", "fuel": "electricity", "match_quality": "confirmed_exact"})
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,variant,region,fuel,match_quality,product_json) VALUES($1,'washer','LG','WM3070HXA','US-120V','US','electricity','confirmed_exact',$2)`, exactID, exactProduct); err != nil {
		t.Fatal(err)
	}
	results, err = store.Search(ctx, Search{Brand: "LG", Model: "WM3070HXA", Fuel: "electricity", Region: &us, Variant: &variant})
	if err != nil || len(results) != 2 || !strings.Contains(string(results[0]), exactID) || !strings.Contains(string(results[1]), familyID) {
		t.Fatalf("exact priority: %d %v %s", len(results), err, results)
	}
	// The reviewed EnerGuide row has no documented variant. Its representative
	// model is searchable, while another model in the printed pattern is only a
	// fact-free family lead, even if the customer supplied a variant.
	energuideID := uuid.NewString()
	energuideProduct, _ := json.Marshal(map[string]any{"id": energuideID, "category": "refrigerator", "brand": "KitchenAid", "full_model": "KSSC48QTS00", "variant": nil, "region": nil, "fuel": "electricity", "match_quality": "documented_variant_family"})
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'refrigerator','KitchenAid','KSSC48QTS00','electricity','documented_variant_family',$2)`, energuideID, energuideProduct); err != nil {
		t.Fatal(err)
	}
	energuideFactID := uuid.NewString()
	energuideFact := map[string]any{"id": energuideFactID, "kind": "annual_energy", "value": "659", "unit": "kWh", "test_basis": map[string]any{"matched_model": "KSSC48QT*0*"}, "included_energy": map[string]any{"status": "not_specified_by_source"}, "source_kind": "other_official", "source_url": "https://example.org/energuide.pdf", "source_hash": hash, "source_hash_basis": "verified_source_document", "source_manifest": "sources/kitchenaid.json", "reviewed_at": "2026-09-30T00:00:00Z", "revision": 1}
	energuideFactJSON, _ := json.Marshal(energuideFact)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',659,'kWh',$4,'{}','other_official','https://example.org/energuide.pdf',$3,'verified_source_document','sources/kitchenaid.json','2026-09-30T00:00:00Z',1,$5)`, energuideFactID, energuideID, hash, `{"matched_model":"KSSC48QT*0*"}`, energuideFactJSON); err != nil {
		t.Fatal(err)
	}
	results, err = store.Search(ctx, Search{Brand: "KitchenAid", Model: "KSSC48QTS00", Fuel: "electricity"})
	if err != nil || len(results) != 1 {
		t.Fatalf("representative EnerGuide search: %d %v", len(results), err)
	}
	var representative struct {
		ID           string            `json:"id"`
		MatchQuality string            `json:"match_quality"`
		QueryMatch   string            `json:"query_match"`
		Facts        []json.RawMessage `json:"facts"`
	}
	if err = json.Unmarshal(results[0], &representative); err != nil || representative.ID != energuideID || representative.MatchQuality != "documented_variant_family" || len(representative.Facts) != 1 || representative.QueryMatch != "" {
		t.Fatalf("representative EnerGuide family rating: %+v %v", representative, err)
	}
	variant = "unknown-finish"
	results, err = store.Search(ctx, Search{Brand: "KitchenAid", Model: "KSSC48QT500", Fuel: "electricity", Variant: &variant})
	if err != nil || len(results) != 1 {
		t.Fatalf("EnerGuide family lead search: %d %v", len(results), err)
	}
	candidate.QueryMatch = ""
	candidate.MatchedModel = ""
	candidate.VariantMatch = ""
	candidate.Facts = nil
	if err = json.Unmarshal(results[0], &candidate); err != nil || candidate.ID != energuideID || candidate.QueryMatch != "documented_family" || candidate.MatchedModel != "KSSC48QT500" || candidate.VariantMatch != "unresolved" || len(candidate.Facts) != 0 {
		t.Fatalf("EnerGuide family lead exposed a rating: %+v %v", candidate, err)
	}
}
