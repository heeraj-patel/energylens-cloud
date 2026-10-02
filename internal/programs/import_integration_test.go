package programs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestImportReviewedRequiresRegisteredAuthorityAndCompleteEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_import_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `INSERT INTO program_sources(source_key,source_kind,authority_name,administrator_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled)
 VALUES('utility-a','utility','Utility A','Utility A','https://utility.example/program',ARRAY['utility.example'],'html','https://utility.example/terms',$1,$2,'reviewer','v1','v1','{}','[]',interval '7 days',20,1000000,true)`, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	evidence := map[string]string{}
	for _, f := range requiredEvidence {
		evidence[f] = "section 2"
	}
	evidence["application_to"] = "section 3"
	input := ReviewedImport{SourceKey: "utility-a", SourceURL: "https://utility.example/program", SourceBytes: []byte("exact official offer bytes"), ContentType: "text/html", SnapshotRef: "operator:artifact-1", ParserVersion: "v1", AdministratorKey: "utility-a", ProgramKey: "washer", GeographyKey: "TX", AdministratorName: "Utility A", Status: "open", Title: "Washer rebate", BenefitKind: "rebate", BenefitTerms: json.RawMessage(`{"amount":"100"}`), EligibilityTerms: json.RawMessage(`{"category":"washer"}`), StackingTerms: json.RawMessage(`{"status":"unknown"}`), Dates: map[string]string{"application_to": "2026-12-31"}, Evidence: evidence, Reviewer: "reviewer", ReviewNote: "Checked offer and source authority", ReviewExpiresAt: now.Add(7 * 24 * time.Hour)}
	bad := input
	bad.SourceURL = "https://attacker.example/program"
	if _, err = ImportReviewed(ctx, pool, bad); err == nil {
		t.Fatal("unregistered host accepted")
	}
	bad = input
	bad.Evidence = map[string]string{"benefit_terms": "section 2"}
	if _, err = ImportReviewed(ctx, pool, bad); err == nil {
		t.Fatal("missing field evidence accepted")
	}
	id, err := ImportReviewed(ctx, pool, input)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM current_verified_program_revisions WHERE id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("verified count %d: %v", count, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE program_source_snapshot_content SET content=decode('00','hex')`); err == nil {
		t.Fatal("source bytes were mutable")
	}
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=now()-interval '1 second' WHERE source_key='utility-a'`); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM current_verified_program_revisions WHERE id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired review visible: %d %v", count, err)
	}
}
