package migrate

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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

func TestFirstDeviceMigrationBackfillsPersistedHistoryOnly(t *testing.T) {
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
	schema := "first_device_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	before := fstest.MapFS{}
	names, err := Files(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name == "0093_first_device_signup.sql" {
			continue
		}
		body, readErr := fs.ReadFile(migrations.FS, name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		before[name] = &fstest.MapFile{Data: body}
	}
	if err = Apply(ctx, conn, before); err != nil {
		t.Fatal(err)
	}
	owner, usedHome, demoHome := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = conn.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$3,'Used','UTC'),($2,$3,'Demo only','UTC')`, usedHome, demoHome, owner); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2025, 5, 2, 1, 2, 3, 0, time.UTC)
	if _, err = conn.Exec(ctx, `INSERT INTO appliances(home_id,name,category,identity,created_at,archived_at)
		VALUES($1,'Archived first','refrigerator','{}',$2,$2),($1,'Active later','tv','{}',$3,NULL)`, usedHome, first, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var backfilled, demoOnly *time.Time
	if err = conn.QueryRow(ctx, `SELECT first_device_added_at FROM homes WHERE id=$1`, usedHome).Scan(&backfilled); err != nil || backfilled == nil || !backfilled.Equal(first) {
		t.Fatalf("persisted history backfill: %v %v", backfilled, err)
	}
	if err = conn.QueryRow(ctx, `SELECT first_device_added_at FROM homes WHERE id=$1`, demoHome).Scan(&demoOnly); err != nil || demoOnly != nil {
		t.Fatalf("empty/demo-only home milestone: %v %v", demoOnly, err)
	}
}
