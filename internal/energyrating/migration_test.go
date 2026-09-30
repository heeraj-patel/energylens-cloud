package energyrating

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Set ENERGYLENS_CLOUD_TEST_DATABASE_URL to exercise the real migration and
// database review gate. The test creates and drops an isolated schema.
func TestRatingCatalogMigrationAndPersistence(t *testing.T) {
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
	schema := "energy_rating_test_" + time.Now().UTC().Format("150405000000")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)

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
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0017_energy_rating_catalog.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply rating migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("reapply rating migration: %v", err)
	}

	store := NewStore(pool)
	const insert = `INSERT INTO energy_rating_catalog (brand, model, category, source_kind, annual_kwh, rating_usage_basis, source_ref, source_row_id, contract_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	if _, err := pool.Exec(ctx, insert, "Lenovo", "20XLS1H500", "computer", "epa_exact", 318.0, "hours_per_day:8", "https://catalog.example/row", "row-1", "energy-v1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.FindReviewed(ctx, "Lenovo", "20XLS1H500", "computer"); err != nil || ok {
		t.Fatalf("unreviewed rating was readable: ok=%v err=%v", ok, err)
	}
	preReviewValue := 319.0
	if err := store.SaveCandidate(ctx, Candidate{Brand: "lenovo", Model: "20xls1h500", Category: "computer", SourceKind: "epa_exact", AnnualKWh: &preReviewValue, SourceRef: "https://catalog.example/row-v2", SourceRowID: ptr("row-pre-review"), ContractVersion: "energy-v1"}); err != nil {
		t.Fatalf("case-variant candidate upsert: %v", err)
	}
	var identityCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM energy_rating_catalog`).Scan(&identityCount); err != nil || identityCount != 1 {
		t.Fatalf("case variants created %d rows (err=%v), want one", identityCount, err)
	}
	reviewedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if promoted, err := store.Review(ctx, "LENOVO", "20XLS1H500", "epa_exact", reviewedAt); err != nil || !promoted {
		t.Fatalf("case-insensitive Review = (%v, %v)", promoted, err)
	}
	updated := 999.0
	if err := store.SaveCandidate(ctx, Candidate{Brand: "LENOVO", Model: "20XLS1H500", Category: "computer", SourceKind: "epa_exact", AnnualKWh: &updated, SourceRef: "https://catalog.example/replacement", SourceRowID: ptr("row-2"), ContractVersion: "energy-v2"}); err != nil {
		t.Fatalf("candidate save over reviewed row: %v", err)
	}
	got, ok, err := store.FindReviewed(ctx, "LeNoVo", "20XLS1H500", "computer")
	if err != nil || !ok || got.SourceKind != "epa_exact" || got.AnnualKWh == nil || *got.AnnualKWh != 319 || got.SourceRowID == nil || *got.SourceRowID != "row-pre-review" {
		t.Fatalf("candidate save changed reviewed evidence: rating=%+v ok=%v err=%v", got, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE energy_rating_catalog SET annual_kwh = 999 WHERE brand = 'Lenovo'`); err == nil {
		t.Fatal("database allowed mutation of reviewed rating")
	}
	if _, err := pool.Exec(ctx, insert, "Other", "Model", "computer", "oem_exact", 1.0, "hours_per_day:0", "https://catalog.example/bad", "row-bad", "energy-v1"); err == nil {
		t.Fatal("migration accepted invalid usage basis")
	}
}

func ptr(s string) *string { return &s }
