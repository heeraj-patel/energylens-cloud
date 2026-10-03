package programs

import (
	"context"
	"errors"
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

type fixedZIPResolver struct {
	scopes []Scope
	err    error
}

func (r fixedZIPResolver) ResolveZIP(context.Context, string, string) ([]Scope, error) {
	return r.scopes, r.err
}

type countingProbe struct {
	calls map[string]int
	fail  string
}

type recordedProbe struct{ calls int }

func (p *recordedProbe) Check(context.Context, RegisteredSource) error {
	return errors.New("unexpected Check call")
}
func (p *recordedProbe) Fetch(_ context.Context, _ RegisteredSource) (FetchedSource, error) {
	p.calls++
	return FetchedSource{Content: []byte("recorded official page"), ContentType: "text/html"}, nil
}

func (p *countingProbe) Check(_ context.Context, s RegisteredSource) error {
	p.calls[s.Key]++
	if s.Key == p.fail {
		return errors.New("unavailable")
	}
	return nil
}

func TestDiscoveryRecordsFullCandidateFanoutAndIncompleteCoverage(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_fanout_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	for _, key := range []string{"grid-a", "grid-b"} {
		_, err = pool.Exec(ctx, `INSERT INTO program_sources(source_key,source_kind,authority_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled)
		VALUES($1,'utility',$1,'https://utility.example/terms',ARRAY['utility.example'],'html','https://utility.example/terms',$2,$3,'reviewer','v1','v1','{}','[]',interval '7 days',1,100000,true)`, key, now.Add(-time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	probe := &countingProbe{calls: map[string]int{}, fail: "grid-b"}
	runner := DiscoveryRunner{DB: pool, Probe: probe, Resolver: fixedZIPResolver{scopes: []Scope{
		{Kind: "utility", Key: "grid-a", Name: "Grid A", ProviderRole: "electric_distributor", SourceKey: "grid-a"},
		{Kind: "utility", Key: "grid-b", Name: "Grid B", ProviderRole: "electric_retailer", SourceKey: "grid-b"},
		{Kind: "utility", Key: "grid-a-gas", Name: "Grid A Gas", ProviderRole: "gas_distributor", SourceKey: "grid-a"},
		{Kind: "county", Key: "travis", Name: "Travis County"},
		{Kind: "state", Key: "TX", Name: "Texas"},
		{Kind: "federal", Key: "US", Name: "United States"},
		{Kind: "tribal", Key: "candidate-tribe", Name: "Candidate Tribe"},
		{Kind: "manufacturer", Key: "oem-a", Name: "OEM A"},
	}}}
	out, err := runner.Run(ctx, DiscoveryRequest{CountryCode: "US", PostalCode: "78732", StateCode: "TX"})
	if err != nil || out.State != "partial" {
		t.Fatalf("run = %+v, %v", out, err)
	}
	if probe.calls["grid-a"] != 1 || probe.calls["grid-b"] != 1 {
		t.Fatalf("unbounded duplicate source checks: %+v", probe.calls)
	}
	var total, checked, failed, pending int
	err = pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='checked'),count(*) FILTER (WHERE status='failed'),count(*) FILTER (WHERE status='pending') FROM program_discovery_coverage WHERE run_id=$1`, out.RunID).Scan(&total, &checked, &failed, &pending)
	if err != nil || total != 8 || checked != 2 || failed != 1 || pending != 5 {
		t.Fatalf("coverage %d/%d/%d/%d: %v", total, checked, failed, pending, err)
	}
	var published int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM current_verified_program_revisions`).Scan(&published); err != nil || published != 0 {
		t.Fatalf("discovery published terms: %d, %v", published, err)
	}
	for _, row := range []struct{ key, kind, scope, role, zip string }{
		{"gas-utility", "utility", "utility", "gas_distributor", "78732"},
		{"state-office", "government", "state", "", "78732"},
		{"appliance-maker", "manufacturer", "manufacturer", "", "78732"},
		{"other-zip", "government", "federal", "", "78733"},
	} {
		geography := `{"country_code":"US","postal_codes":["` + row.zip + `"],"scope_kind":"` + row.scope + `","provider_role":"` + row.role + `"}`
		_, err = pool.Exec(ctx, `INSERT INTO program_sources(source_key,source_kind,authority_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled)
		VALUES($1,$2,$1,'https://official.example/terms',ARRAY['official.example'],'html','https://official.example/terms',$3,$4,'reviewer','v1','v1',$5::jsonb,'["washer"]',interval '7 days',1,100000,true)`, row.key, row.kind, now.Add(-time.Hour), now.Add(time.Hour), geography)
		if err != nil {
			t.Fatal(err)
		}
	}
	recorded := &recordedProbe{}
	registeredRun, err := (DiscoveryRunner{DB: pool, Resolver: fixedZIPResolver{}, Probe: recorded}).Run(ctx, DiscoveryRequest{CountryCode: "US", PostalCode: "78732", Category: "washer"})
	if err != nil || registeredRun.State != "partial" || recorded.calls != 3 {
		t.Fatalf("registered fanout: %+v, calls=%d, err=%v", registeredRun, recorded.calls, err)
	}
	var registeredCount, snapshotCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM program_discovery_coverage WHERE run_id=$1 AND status='checked'`, registeredRun.RunID).Scan(&registeredCount); err != nil || registeredCount != 3 {
		t.Fatalf("registered coverage: %d, %v", registeredCount, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM program_source_snapshots s JOIN program_source_snapshot_content c ON c.snapshot_id=s.id WHERE c.content=$1`, []byte("recorded official page")).Scan(&snapshotCount); err != nil || snapshotCount != 3 {
		t.Fatalf("immutable source bytes: %d, %v", snapshotCount, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM public_program_revisions`).Scan(&published); err != nil || published != 0 {
		t.Fatalf("unreviewed snapshots published terms: %d, %v", published, err)
	}
	failedRun, err := (DiscoveryRunner{DB: pool, Resolver: fixedZIPResolver{err: errors.New("lookup down")}}).Run(ctx, DiscoveryRequest{CountryCode: "US", PostalCode: "78732"})
	if err != nil || failedRun.State != "failed" {
		t.Fatalf("failed resolver run = %+v, %v", failedRun, err)
	}
	var limitation string
	if err = pool.QueryRow(ctx, `SELECT limitations->>0 FROM program_discovery_runs WHERE id=$1`, failedRun.RunID).Scan(&limitation); err != nil || limitation != "zip_resolver_failed" {
		t.Fatalf("limitation = %q, %v", limitation, err)
	}
}

func TestOEDIDiscoveryPersistsPartialCoverageAndHashes(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "oedi_zip_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	r := oediFixtureResolver(t)
	out, err := (DiscoveryRunner{DB: pool, Resolver: r}).Run(ctx, DiscoveryRequest{CountryCode: "US", PostalCode: "92503"})
	if err != nil || out.State != "partial" {
		t.Fatalf("run: %+v, %v", out, err)
	}
	var total, pending, linked int
	err = pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='pending' AND provider_role='electric_service_candidate'),count(*) FILTER (WHERE source_id IS NOT NULL) FROM program_discovery_coverage WHERE run_id=$1`, out.RunID).Scan(&total, &pending, &linked)
	if err != nil || total != 3 || pending != 3 || linked != 0 {
		t.Fatalf("coverage %d/%d/%d: %v", total, pending, linked, err)
	}
	var year, digest, limitation string
	err = pool.QueryRow(ctx, `SELECT resolution_provenance->>'source_data_year',resolution_provenance->'source_sha256'->>'iou_zipcodes_2024.csv',limitations->>0 FROM program_discovery_runs WHERE id=$1`, out.RunID).Scan(&year, &digest, &limitation)
	if err != nil || year != "2024" || digest != r.IOUSHA256 || limitation != "electricity_only" {
		t.Fatalf("provenance %s/%s/%s: %v", year, digest, limitation, err)
	}
	var programs int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM public_programs`).Scan(&programs); err != nil || programs != 0 {
		t.Fatalf("programs = %d: %v", programs, err)
	}
}
