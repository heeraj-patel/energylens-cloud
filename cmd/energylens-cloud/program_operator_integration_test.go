package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
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

func TestImportReviewedProgramOperatorArtifact(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_operator_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
 VALUES('doe-wap-test','government','U.S. Department of Energy','U.S. Department of Energy','https://www.energy.gov/cmei/scep/wap/how-apply-weatherization-assistance',ARRAY['www.energy.gov'],'manual','https://www.energy.gov/', $1,$2,'fixture-reviewer','manual-v1','v1','{}','[]',interval '365 days',1,1000000,true)`, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// An official-page excerpt is a test artifact; the test makes no claim about
	// current funding or a household award.
	source := []byte("The U.S. Department of Energy's (DOE) Weatherization Assistance Program (WAP) is administered at the state and local level. One of the primary factors affecting eligibility is income. After receiving your application and income information, your local weatherization provider will determine if you are income eligible to receive weatherization services.")
	digest := sha256.Sum256(source)
	evidence := map[string]string{}
	for _, field := range []string{"administrator_kind", "title", "status", "benefit_kind", "benefit_display", "benefit_terms", "eligibility_terms", "stacking_terms", "applicability.country_code", "applicability.postal_codes", "applicability.geography_scope", "applicability.category", "applicability.fuel", "applicability.provider_key", "applicability.provider_role", "applicability.requires_income", "applicability.requires_address", "applicability.additional_conditions"} {
		evidence[field] = "official WAP application excerpt; scope and unknowns reviewed with full source"
	}
	artifact := reviewedProgramArtifact{ReviewedImport: programs.ReviewedImport{
		SourceKey: "doe-wap-test", SourceURL: "https://www.energy.gov/cmei/scep/wap/how-apply-weatherization-assistance", ContentType: "text/plain", SnapshotRef: "operator:fixture-wap", ParserVersion: "manual-v1",
		AdministratorKey: "doe", ProgramKey: "wap-test", GeographyKey: "US", AdministratorName: "U.S. Department of Energy", AdministratorKind: "federal", Status: "open", Title: "Weatherization Assistance Program", BenefitKind: "other", BenefitDisplay: "Weatherization services after local provider review", BenefitTerms: json.RawMessage(`{"kind":"conditional_in_kind_service"}`), EligibilityTerms: json.RawMessage(`{"income":"local_provider_review","capacity":"unknown"}`), StackingTerms: json.RawMessage(`{"status":"unknown"}`), Evidence: evidence, Reviewer: "fixture-reviewer", ReviewNote: "Fixture tests an offline reviewed import, not live availability", ReviewExpiresAt: now.Add(7 * 24 * time.Hour), Applicability: &programs.StructuredApplicability{CountryCode: "US", GeographyScope: "country", Category: "home", Fuel: "any", RequiresIncome: true, AdditionalConditions: true},
	}, SourceSHA256: hex.EncodeToString(digest[:])}
	tmp := t.TempDir()
	artifactPath, sourcePath := filepath.Join(tmp, "review.json"), filepath.Join(tmp, "source.txt")
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(artifactPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(sourcePath, source, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := importReviewedProgram(ctx, pool, flag.NewFlagSet("test", flag.ContinueOnError), []string{"--artifact", artifactPath, "--source-snapshot", sourcePath})
	if err != nil || result["revision_id"] == "" {
		t.Fatalf("operator import: %+v %v", result, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM current_verified_program_revisions WHERE id=$1`, result["revision_id"]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("verified source-backed fixture: %d %v", count, err)
	}
	if err = os.WriteFile(sourcePath, []byte("changed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = importReviewedProgram(ctx, pool, flag.NewFlagSet("test", flag.ContinueOnError), []string{"--artifact", artifactPath, "--source-snapshot", sourcePath}); err == nil {
		t.Fatal("changed source bytes accepted")
	}
}
