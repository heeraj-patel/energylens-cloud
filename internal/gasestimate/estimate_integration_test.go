package gasestimate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestForApplianceReadsLinkedFactsIncludingZeroKWh(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "gasestimate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
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
	hash := strings.Repeat("d", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash); err != nil {
		t.Fatal(err)
	}
	n := 0
	seed := func(category, fuel, model string, facts map[[2]string]float64, basis string) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,$2,'Acme',$3,$4,'confirmed_exact','{}')`, id, category, model, fuel); err != nil {
			t.Fatal(err)
		}
		for ku, v := range facts {
			n++
			fact, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "kind": ku[0], "value": v, "unit": ku[1]})
			if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
			VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,'{}','other_official','https://example.org/f',$8,'reviewed_row_manifest','m','2026-10-01T00:00:00Z',1,$9)`,
				uuid.NewString(), id, hash, ku[0], v, ku[1], basis, fmt.Sprintf("%064x", n), fact); err != nil {
				t.Fatalf("%v: %v", ku, err)
			}
		}
		return id
	}
	wh := seed("water_heater", "natural_gas", "WH1", map[[2]string]float64{{"annual_energy", "kWh"}: 0, {"annual_fuel", "therm"}: 178}, "{}")
	e, ok, err := ForAppliance(ctx, pool, "Water Heater", "natural_gas", &wh)
	if err != nil || !ok {
		t.Fatalf("water heater: %v %v", ok, err)
	}
	near(t, "electric", e.ElectricKWh, 0, 0)
	near(t, "therms", e.GasTherms, 178, 0)
	near(t, "total", e.TotalKWhEq, 178*29.31, 1e-9)
	if e.GasBasis != BasisRatedFact || e.ElectricBasis != BasisRatedFact {
		t.Fatalf("basis %q/%q", e.GasBasis, e.ElectricBasis)
	}

	dryer := seed("dryer", "natural_gas", "DVG45T6000", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, `{"appendix":"D1"}`)
	e, ok, err = ForAppliance(ctx, pool, "dryer", "natural_gas", &dryer)
	if err != nil || !ok {
		t.Fatalf("dryer: %v %v", ok, err)
	}
	near(t, "dryer total", e.TotalKWhEq, 724.7, 0.1)
	near(t, "dryer therms", e.GasTherms, 22.7, 0.1)

	e, ok, err = ForAppliance(ctx, pool, "dryer", "natural_gas", nil)
	if err != nil || !ok {
		t.Fatalf("unlinked: %v %v", ok, err)
	}
	near(t, "unlinked electric typical", e.ElectricKWh, 59.43, 0.01)
	isNil(t, "unlinked therms", e.GasTherms)

	missing := uuid.NewString()
	if _, ok, err = ForAppliance(ctx, pool, "dryer", "natural_gas", &missing); err != nil || !ok {
		t.Fatalf("unknown product: %v %v", ok, err)
	}
	if _, ok, err = ForAppliance(ctx, pool, "dryer", "electricity", &dryer); err != nil || ok {
		t.Fatalf("electric: %v %v", ok, err)
	}
}
