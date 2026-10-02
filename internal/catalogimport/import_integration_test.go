package catalogimport

import (
	"context"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestImportPublicationTransactionAndReplay(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "catalog_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	raw, key, _ := fixture(t)
	verified, err := ParseAndVerify(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Import(ctx, pool, verified, "test-operator")
	if err != nil || result.Replayed || result.Products != 1 || result.Facts != 1 {
		t.Fatalf("first import %+v %v", result, err)
	}
	replay, err := Import(ctx, pool, verified, "test-operator")
	if err != nil || !replay.Replayed {
		t.Fatalf("replay %+v %v", replay, err)
	}
	var fetched *string
	if err = pool.QueryRow(ctx, `SELECT source_fetched_at::text FROM catalog_fact_revisions LIMIT 1`).Scan(&fetched); err != nil || fetched != nil {
		t.Fatalf("source fetch time invented: %v %v", fetched, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE catalog_fact_revisions SET value=999`); err == nil {
		t.Fatal("immutable fact updated")
	}
	conflicting := verified
	conflicting.PayloadHash = strings.Repeat("b", 64)
	conflicting.Envelope.Payload.Products[0].Brand = "Different"
	if _, err = Import(ctx, pool, conflicting, "test-operator"); err == nil {
		t.Fatal("conflicting product accepted")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_publications`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("conflict did not roll back publication: %d %v", count, err)
	}
	differentEnvelope := verified
	differentEnvelope.EnvelopeHash = strings.Repeat("c", 64)
	if _, err = Import(ctx, pool, differentEnvelope, "test-operator"); err == nil {
		t.Fatal("different replay accepted")
	}
}
