package usagefactimport

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

func TestPostgresUsageImportReplayConflictAndImmutability(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	p, der, priv, key := fixture(t)
	verified, err := ParseAndVerify(signFixture(t, p, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Import(ctx, pool, verified, "operator")
	if err != nil || result.Replayed || result.Facts != 1 {
		t.Fatalf("first import: %+v %v", result, err)
	}
	replay, err := Import(ctx, pool, verified, "operator")
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var annual, usage int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_fact_revisions`).Scan(&annual); err != nil || annual != 0 {
		t.Fatalf("annual facts changed: %d %v", annual, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_facts`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("usage rows: %d %v", usage, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE reviewed_usage_facts SET value=9`); err == nil {
		t.Fatal("immutable fact updated")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM reviewed_usage_publications`); err == nil {
		t.Fatal("immutable publication deleted")
	}
	conflict := p
	conflict.Facts = append([]Fact(nil), p.Facts...)
	conflict.Facts[0].Value = "0.43"
	changed, err := ParseAndVerify(signFixture(t, conflict, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, changed, "operator"); err == nil {
		t.Fatal("conflicting immutable ID accepted")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("conflict did not roll back publication: %d %v", usage, err)
	}
	alias := p
	alias.Facts = append([]Fact(nil), p.Facts...)
	alias.Facts[0].ID = "22222222-2222-5222-8222-222222222222"
	alias.Facts[0].Model = "WM-3070-HWA"
	aliased, err := ParseAndVerify(signFixture(t, alias, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, aliased, "operator"); err == nil {
		t.Fatal("ambiguous normalized model identity accepted")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("ambiguous identity did not roll back publication: %d %v", usage, err)
	}
	extended := p
	extended.Facts = append([]Fact(nil), p.Facts...)
	second := p.Facts[0]
	second.ID = "33333333-3333-5333-8333-333333333333"
	second.Model = "WM3070HXA"
	extended.Facts = append(extended.Facts, second)
	extendedVerified, err := ParseAndVerify(signFixture(t, extended, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, extendedVerified, "operator"); err == nil {
		t.Fatal("same immutable fact joined a second publication")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("repeated fact did not roll back publication: %d %v", usage, err)
	}
	altered := verified
	altered.EnvelopeHash = strings.Repeat("c", 64)
	if _, err = Import(ctx, pool, altered, "operator"); err == nil {
		t.Fatal("mutated verified envelope accepted")
	}
}
