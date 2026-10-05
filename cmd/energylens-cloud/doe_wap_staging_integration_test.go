package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run with a verified private DOE capture and generated staging artifact. The
// official source bytes are intentionally not committed to the repository.
func TestDOEWAPStagingOwnerFeed(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	sourcePath := os.Getenv("ENERGYLENS_WAP_CAPTURE_PATH")
	artifactPath := os.Getenv("ENERGYLENS_WAP_STAGING_ARTIFACT")
	if dsn == "" || sourcePath == "" || artifactPath == "" {
		t.Skip("staging database, private DOE capture and generated artifact required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var dbName string
	if err = admin.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil || !strings.HasPrefix(dbName, "energylens_program_staging_") {
		t.Fatalf("staging database name required: %q %v", dbName, err)
	}
	schema := "wap_packet_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	registration, err := os.ReadFile(filepath.Join("..", "..", "runbooks", "staging", "doe-wap-2026-10-05", "register-local.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, string(registration)); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	result, err := importReviewedProgram(ctx, pool, flag.NewFlagSet("staging", flag.ContinueOnError), []string{"--artifact", artifactPath, "--source-snapshot", sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	var owner, home uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Staging Home','America/Chicago') RETURNING id`, owner).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	got, err := (programs.PGOwnerStore{Pool: pool}).ListHome(ctx, owner.String(), home.String(), time.Now().UTC())
	if err != nil || len(got.Programs) != 1 || got.Programs[0].RevisionID != result["revision_id"] || got.Programs[0].SourceSHA256 != result["source_sha256"] || got.Programs[0].AvailabilityStatus != "open" || got.Programs[0].Status != "unknown" || !slices.Contains(got.Programs[0].Reasons, "income_unknown") || !slices.Contains(got.Programs[0].Reasons, "address_unknown") || !slices.Contains(got.Programs[0].Reasons, "additional_conditions_unknown") {
		t.Fatalf("staging owner feed must preserve uncertainty: %+v %v", got, err)
	}
	if got.Programs[0].SourceURL != "https://www.energy.gov/cmei/scep/wap/how-apply-weatherization-assistance" {
		t.Fatalf("wrong source URL: %+v", got.Programs[0])
	}
	if got.Programs[0].Title != "Weatherization Assistance Program" || got.Programs[0].BenefitDisplay != "Weatherization services" || got.Programs[0].AdministratorKind != "federal" || got.Programs[0].AdministratorName != "U.S. Department of Energy" {
		t.Fatalf("owner-approved staging display fields: %+v", got.Programs[0])
	}
}
