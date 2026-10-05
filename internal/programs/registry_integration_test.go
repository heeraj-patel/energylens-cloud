package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestReviewedProgramRequiresFreshUnconflictedEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	now := time.Now().UTC()
	var sourceID, snapshotA, snapshotB, programID, reviewedID uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO program_sources
		(source_key, source_kind, authority_name, administrator_name, canonical_url, allowed_hosts,
		 access_method, terms_url, terms_reviewed_at, terms_review_expires_at, terms_reviewed_by,
		 parser_version, canonicalization_version, geography_coverage, category_coverage,
		 refresh_interval, max_requests_per_run, max_response_bytes)
		VALUES ('utility-a', 'utility', 'Utility A', 'Utility A', 'https://utility.example/program',
		 ARRAY['utility.example'], 'html', 'https://utility.example/terms', $1, $2, 'reviewer',
		 'v1', 'v1', '{"state":"TX"}', '["washer"]', interval '7 days', 20, 1000000)
		RETURNING id`, now.Add(-time.Hour), now.Add(time.Hour)).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	contentA, contentB := []byte("official program terms A"), []byte("official program terms B")
	hashA, hashB := sha256.Sum256(contentA), sha256.Sum256(contentB)
	insertSnapshot := `INSERT INTO program_source_snapshots
		(source_id, source_url, content_sha256, content_type, content_bytes, fetched_at, parser_version, snapshot_ref)
		VALUES ($1, $2, $3, 'text/html', $6, $4, 'v1', $5) RETURNING id`
	if err = pool.QueryRow(ctx, insertSnapshot, sourceID, "https://utility.example/program",
		hex.EncodeToString(hashA[:]), now.Add(-time.Hour), "sha256:a", len(contentA)).Scan(&snapshotA); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, insertSnapshot, sourceID, "https://utility.example/program",
		hex.EncodeToString(hashB[:]), now.Add(-30*time.Minute), "sha256:b", len(contentB)).Scan(&snapshotB); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		id      uuid.UUID
		content []byte
	}{{snapshotA, contentA}, {snapshotB, contentB}} {
		if _, err = pool.Exec(ctx, `INSERT INTO program_source_snapshot_content(snapshot_id,content) VALUES($1,$2)`, entry.id, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err = pool.QueryRow(ctx, `INSERT INTO public_programs
		(administrator_key, program_key, geography_key, administrator_name, source_id)
		VALUES ('utility-a','washer-2026','TX:78732','Utility A',$1) RETURNING id`, sourceID).Scan(&programID); err != nil {
		t.Fatal(err)
	}
	insertRevision := `INSERT INTO public_program_revisions
		(program_id, revision, source_snapshot_id, lifecycle, administrator_kind, status, title, benefit_kind,
		 benefit_terms, eligibility_terms, stacking_terms, extracted_at, reviewed_at,
		 review_expires_at, reviewed_by)
		VALUES ($1,$2,$3,$4,'utility','open','Washer offer','rebate','{}','{}','{}',$5,$6,$7,$8) RETURNING id`
	if err = pool.QueryRow(ctx, insertRevision, programID, 1, snapshotA, "candidate", now.Add(-time.Hour),
		nil, nil, nil).Scan(&reviewedID); err != nil {
		t.Fatal(err)
	}
	assertCount := func(want int) {
		t.Helper()
		var got int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM current_verified_program_revisions WHERE program_id=$1`, programID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("verified revisions: got %d, want %d", got, want)
		}
	}
	assertCount(0) // Extraction alone is not a review.
	if _, err = pool.Exec(ctx, `INSERT INTO public_program_evidence(revision_id,field_path,snapshot_id,locator)
		VALUES ($1,'benefit_terms',$2,'section 2')`, reviewedID, snapshotA); err != nil {
		t.Fatal(err)
	}
	var acceptedID uuid.UUID
	if err = pool.QueryRow(ctx, insertRevision, programID, 2, snapshotA, "reviewed", now.Add(-time.Hour),
		now.Add(-time.Minute), now.Add(time.Hour), "reviewer").Scan(&acceptedID); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // Accepted terms still need field evidence on that revision.
	for _, field := range []string{"title", "status", "benefit_kind", "benefit_terms", "eligibility_terms", "stacking_terms"} {
		if _, err = pool.Exec(ctx, `INSERT INTO public_program_evidence(revision_id,field_path,snapshot_id,locator)
			VALUES ($1,$2,$3,'section 2')`, acceptedID, field, snapshotA); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(0) // Reviewed terms alone do not establish administrator scope.
	if _, err = pool.Exec(ctx, `INSERT INTO public_program_evidence(revision_id,field_path,snapshot_id,locator)
		VALUES ($1,'administrator_kind',$2,'section 1')`, acceptedID, snapshotA); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	var conflictID uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO public_program_conflicts
		(revision_id,field_path,first_snapshot_id,second_snapshot_id,details)
		VALUES ($1,'benefit_terms',$2,$3,'different caps') RETURNING id`, acceptedID, snapshotA, snapshotB).Scan(&conflictID); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // Contradictory source terms suppress current claims.
	if _, err = pool.Exec(ctx, `INSERT INTO public_program_conflict_resolutions
		(conflict_id,controlling_snapshot_id,resolved_at,resolved_by,rationale)
		VALUES ($1,$2,$3,'reviewer','Administrator terms control')`, conflictID, snapshotA, now); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=$2 WHERE id=$1`, sourceID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // A successful fetch cannot renew the source terms review.
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=$2 WHERE id=$1`, sourceID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	var nextID uuid.UUID
	if err = pool.QueryRow(ctx, insertRevision, programID, 3, snapshotB, "candidate", now,
		nil, nil, nil).Scan(&nextID); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // New extraction invalidates the older reviewed revision.
	if _, err = pool.Exec(ctx, `INSERT INTO public_program_revisions
		(program_id,revision,source_snapshot_id,lifecycle,administrator_kind,status,title,benefit_kind,
		 benefit_terms,eligibility_terms,stacking_terms,extracted_at,reviewed_at,review_expires_at,reviewed_by)
		 VALUES($1,4,$2,'reviewed','unknown','open','Washer offer','rebate','{}','{}','{}',$3,$3,$4,'reviewer')`, programID, snapshotB, now, now.Add(time.Hour)); err == nil {
		t.Fatal("unknown administrator kind accepted by storage")
	}
	var unclassifiedID uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO public_program_revisions
		(program_id,revision,source_snapshot_id,lifecycle,status,title,benefit_kind,
		 benefit_terms,eligibility_terms,stacking_terms,extracted_at,reviewed_at,review_expires_at,reviewed_by)
		 VALUES($1,4,$2,'reviewed','open','Washer offer','rebate','{}','{}','{}',$3,$3,$4,'reviewer') RETURNING id`, programID, snapshotB, now, now.Add(time.Hour)).Scan(&unclassifiedID); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"administrator_kind", "title", "status", "benefit_kind", "benefit_terms", "eligibility_terms", "stacking_terms"} {
		if _, err = pool.Exec(ctx, `INSERT INTO public_program_evidence(revision_id,field_path,snapshot_id,locator)
			VALUES($1,$2,$3,'section 1')`, unclassifiedID, field, snapshotB); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(0) // Legacy or unclassified reviews never enter the owner feed.
	if _, err = pool.Exec(ctx, `DELETE FROM public_program_revisions WHERE id=$1`, acceptedID); err == nil {
		t.Fatal("accepted source evidence was mutable")
	}
}

func TestProgramCoverageRetainsCompetingUtilitiesAndFailures(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_coverage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var runID uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO program_discovery_runs(country_code,postal_code,state_code,state)
		VALUES ('US','78732','TX','partial') RETURNING id`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO program_discovery_coverage
		(run_id,scope_kind,scope_key,scope_name,provider_role,status,checked_at,failure_code)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	for _, row := range [][]any{
		{runID, "utility", "utility-a", "Utility A", "electric_distributor", "checked", time.Now(), nil},
		{runID, "utility", "utility-b", "Utility B", "electric_retailer", "identified", nil, nil},
		{runID, "county", "travis", "Travis County", nil, "failed", nil, "source_timeout"},
		{runID, "state", "TX", "Texas", nil, "pending", nil, nil},
		{runID, "federal", "US", "United States", nil, "checked", time.Now(), nil},
		{runID, "manufacturer", "oem-a", "OEM A", nil, "pending", nil, nil},
	} {
		if _, err = pool.Exec(ctx, insert, row...); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM program_discovery_coverage WHERE run_id=$1`, runID).Scan(&count); err != nil || count != 6 {
		t.Fatalf("coverage count %d: %v", count, err)
	}
	if _, err = pool.Exec(ctx, insert, runID, "state", "TX", "Texas", nil, "pending", nil, nil); err == nil {
		t.Fatal("duplicate jurisdiction coverage was accepted")
	}
}
