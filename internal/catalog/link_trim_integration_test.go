package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type trimSeed struct {
	brand, model, quality, source string
	kwh                           float64
	age                           time.Duration
}

func seedTrim(t *testing.T, pool *pgxpool.Pool, seeds ...trimSeed) map[string]string {
	t.Helper()
	ctx := context.Background()
	hash := strings.Repeat("e", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0)`, hash); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, s := range seeds {
		id := uuid.NewString()
		ids[s.model] = id
		product, _ := json.Marshal(map[string]any{"brand": s.brand, "brand_key": BrandKey(s.brand), "full_model": s.model})
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'refrigerator',$2,$3,'electricity',$4,$5)`, id, s.brand, s.model, s.quality, product); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
		VALUES($1,$2,$3,'annual_energy',$4,'kWh',jsonb_build_object('matched_model',$5::text),'{}',$6,'https://example.org/f',$3,'source_row','m',now()-$7::interval,1,'{}')`,
			uuid.NewString(), id, hash, s.kwh, s.model, s.source, s.age.String()); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestFindLinkPrefixTrim(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_trim_")
	ids := seedTrim(t, pool,
		trimSeed{brand: "KitchenAid", model: "KSSC48QVS**", quality: "confirmed_exact", source: "epa_exact", kwh: 700},
		trimSeed{brand: "KitchenAid", model: "KRMF706ESS", quality: "confirmed_exact", source: "epa_exact", kwh: 600},
		trimSeed{brand: "KitchenAid", model: "KRMF706EBS", quality: "confirmed_exact", source: "epa_exact", kwh: 640},
		trimSeed{brand: "KitchenAid", model: "KRMF706EWH", quality: "confirmed_exact", source: "epa_exact", kwh: 690},
		trimSeed{brand: "KitchenAid", model: "KBSN608EBS", quality: "confirmed_exact", source: "epa_exact", kwh: 500},
		trimSeed{brand: "KitchenAid", model: "KBSN608EWH", quality: "confirmed_exact", source: "epa_exact", kwh: 800},
		trimSeed{brand: "LG", model: "LRMVC2306S", quality: "confirmed_exact", source: "epa_exact", kwh: 716},
		trimSeed{brand: "KitchenAid", model: "KSSC42QVS**", quality: "documented_variant_family", source: "epa_family", kwh: 752},
	)
	cases := []struct {
		name, brand, category, model, want string
	}{
		{"k=0 model is prefix of wildcard model", "KitchenAid", "refrigerator", "KSSC48QVS", "KSSC48QVS**"},
		{"k=1 trim", "KitchenAid", "refrigerator", "KSSC48QVSX", "KSSC48QVS**"},
		{"k=2 trim wildcards", "KitchenAid", "refrigerator", "KSSC48QVSXY", "KSSC48QVS**"},
		{"agreeing candidates link to median", "KitchenAid", "refrigerator", "KRMF706XYZ", "KRMF706EBS"},
		{"disagreeing candidates stay unresolved", "KitchenAid", "refrigerator", "KBSN608XYZ", ""},
		{"stops at six characters", "KitchenAid", "refrigerator", "KSSC4XXXXX", ""},
		{"six characters still trims", "KitchenAid", "refrigerator", "KRMF70XXXX", "KRMF706EBS"},
		{"computer is not trimmed", "KitchenAid", "computer", "KSSC48QVS", ""},
		{"tv is not trimmed", "KitchenAid", "TV", "KSSC48QVSX", ""},
		{"brand alias", "LG Electronics", "refrigerator", "LRMVC2306SXX", "LRMVC2306S"},
		{"other brand is not matched", "Whirlpool", "refrigerator", "LRMVC2306SXX", ""},
		{"model shorter than six", "KitchenAid", "refrigerator", "KSSC4", ""},
		{"exact match wins", "KitchenAid", "refrigerator", "KRMF706ESS", "KRMF706ESS"},
		{"documented pattern wins", "KitchenAid", "refrigerator", "KSSC42QVS12", "KSSC42QVS**"},
	}
	for _, c := range cases {
		link, found, err := FindLink(context.Background(), pool, Search{Brand: c.brand, Model: c.model, Fuel: "electricity", Category: c.category})
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if found {
				t.Errorf("%s: unexpected link %+v", c.name, link)
			}
			continue
		}
		wantQuality := "documented_variant_family"
		if c.model == c.want {
			wantQuality = "confirmed_exact"
		}
		if !found || link.ProductID != ids[c.want] || link.MatchQuality != wantQuality {
			t.Errorf("%s: want %s %s, got %+v found=%v", c.name, wantQuality, c.want, link, found)
		}
	}
}

func TestFindLinkPrefixTrimMedianAndDuplicates(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_trim_")
	ids := seedTrim(t, pool,
		trimSeed{brand: "Frigidaire", model: "FFHT1826AA", quality: "confirmed_exact", source: "epa_exact", kwh: 400},
		trimSeed{brand: "Frigidaire", model: "FFHT1826AB", quality: "confirmed_exact", source: "epa_exact", kwh: 420},
		trimSeed{brand: "Frigidaire", model: "FFHT1826AC", quality: "confirmed_exact", source: "epa_exact", kwh: 440},
		trimSeed{brand: "Frigidaire", model: "FFHT1826AD", quality: "confirmed_exact", source: "epa_exact", kwh: 460},
		trimSeed{brand: "Frigidaire", model: "FFEF3054TS", quality: "confirmed_exact", source: "epa_exact", kwh: 300},
		trimSeed{brand: "Frigidaire", model: "FFEF-3054TS", quality: "confirmed_exact", source: "epa_exact", kwh: 310, age: time.Hour},
	)
	link, found, err := FindLink(context.Background(), pool, Search{Brand: "Frigidaire", Model: "FFHT1826XX", Fuel: "electricity", Category: "refrigerator"})
	if err != nil || !found || link.ProductID != ids["FFHT1826AB"] {
		t.Errorf("even count lower-middle: got %+v found=%v err=%v", link, found, err)
	}
	if _, found, err = FindLink(context.Background(), pool, Search{Brand: "Frigidaire", Model: "FFEF3054TX", Fuel: "electricity", Category: "refrigerator"}); err != nil || !found {
		t.Errorf("duplicate products must count as one: found=%v err=%v", found, err)
	}
}

func TestFindLinkDocumentedFamilyFromDatawizSources(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_trim_")
	ids := seedTrim(t, pool,
		trimSeed{brand: "LG", model: "OLED55C3**", quality: "documented_variant_family", source: "doe_ccd", kwh: 250},
		trimSeed{brand: "Sony", model: "XBRA80K**", quality: "documented_variant_family", source: "oem_search", kwh: 260},
		trimSeed{brand: "Vizio", model: "VFDA50D**", quality: "documented_variant_family", source: "manual_research", kwh: 270},
	)
	for _, c := range []struct{ brand, model, want string }{
		{"LG", "OLED55C3PU", "OLED55C3**"}, {"Sony", "XBRA80KAB", "XBRA80K**"}, {"Vizio", "VFDA50DXY", "VFDA50D**"},
	} {
		link, found, err := FindLink(context.Background(), pool, Search{Brand: c.brand, Model: c.model, Fuel: "electricity", Category: "tv"})
		if err != nil || !found || link.ProductID != ids[c.want] || link.MatchQuality != "documented_variant_family" {
			t.Errorf("%s %s: got %+v found=%v err=%v", c.brand, c.model, link, found, err)
		}
	}
}

func TestFindLinkTrimKeepsExactAndWildcardProductsApart(t *testing.T) {
	pool := migratedCatalogPool(t, "catalog_trim_")
	seedTrim(t, pool,
		trimSeed{brand: "Zenith", model: "ABC1234", quality: "confirmed_exact", source: "epa_exact", kwh: 700, age: time.Hour},
		trimSeed{brand: "Zenith", model: "ABC1234*", quality: "documented_variant_family", source: "epa_family", kwh: 400},
	)
	if link, found, err := FindLink(context.Background(), pool, Search{Brand: "Zenith", Model: "ABC123", Fuel: "electricity", Category: "refrigerator"}); err != nil || found {
		t.Fatalf("exact and wildcard products that disagree must not link: %+v found=%v err=%v", link, found, err)
	}
}
