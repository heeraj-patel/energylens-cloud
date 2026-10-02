package migrate

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real upgrade must retain rate and reviewed rating rows from the existing
// EnergyLens schema while applying the new account/catalog/inventory DDL.
func TestUpgradeFromRateAndRatingMigrationsPreservesRows(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	initial := fstest.MapFS{}
	for _, name := range []string{"0016_energy_rate_cache.sql", "0017_energy_rating_catalog.sql"} {
		body, readErr := fs.ReadFile(migrations.FS, name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		initial[name] = &fstest.MapFile{Data: body}
	}
	if err = Apply(ctx, conn, initial); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO energy_rate_cache(region,rate_usd_per_kwh,period) VALUES('IL',0.16,'2026-09-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO energy_rating_catalog(brand,model,category,source_kind,annual_kwh,rating_usage_basis,source_ref,contract_version) VALUES('KitchenAid','KSSC48QTS00','refrigerator','oem_family',659,'continuous','https://example.test/reviewed','v1')`); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	if err = Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("reapply failed: %v", err)
	}
	var rate string
	if err = conn.QueryRow(ctx, `SELECT rate_usd_per_kwh::text FROM energy_rate_cache WHERE region='IL'`).Scan(&rate); err != nil || rate != "0.16" {
		t.Fatalf("rate row lost or changed: %q, %v", rate, err)
	}
	var annual string
	if err = conn.QueryRow(ctx, `SELECT annual_kwh::text FROM energy_rating_catalog WHERE model='KSSC48QTS00'`).Scan(&annual); err != nil || annual != "659" {
		t.Fatalf("rating row lost or changed: %q, %v", annual, err)
	}
	names, err := Files(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM energylens_schema_migrations`).Scan(&applied); err != nil || applied != len(names) {
		t.Fatalf("migration ledger has %d rows, want %d: %v", applied, len(names), err)
	}
}
