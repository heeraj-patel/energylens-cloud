package programs

import (
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerReviewedProgramBoundaries(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_owner_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var owner, foreign, home, appliance uuid.UUID
	for _, v := range []*uuid.UUID{&owner, &foreign} {
		if err = pool.QueryRow(ctx, "INSERT INTO users DEFAULT VALUES RETURNING id").Scan(v); err != nil {
			t.Fatal(err)
		}
	}
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Home','America/Chicago') RETURNING id`, owner).Scan(&home); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home)
	if err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Washer','washer','{"fuel":"electricity"}') RETURNING id`, home).Scan(&appliance); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `INSERT INTO program_sources(source_key,source_kind,authority_name,administrator_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled) VALUES('utility-a','utility','Utility A','Utility A','https://utility.example/program',ARRAY['utility.example'],'html','https://utility.example/terms',$1,$2,'reviewer','v1','v1','{}','[]',interval '7 days',20,1000000,true)`, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	evidence := map[string]string{}
	for _, f := range requiredEvidence {
		evidence[f] = "section 1"
	}
	for _, f := range []string{"country_code", "postal_codes", "category", "fuel", "provider_key", "provider_role", "requires_income", "requires_address", "additional_conditions"} {
		evidence["applicability."+f] = "section 2"
	}
	input := ReviewedImport{SourceKey: "utility-a", SourceURL: "https://utility.example/program", SourceBytes: []byte("source terms"), ContentType: "text/html", SnapshotRef: "test-source", ParserVersion: "v1", AdministratorKey: "utility-a", ProgramKey: "washer", GeographyKey: "78732", AdministratorName: "Utility A", Status: "open", Title: "Washer rebate", BenefitKind: "rebate", BenefitTerms: json.RawMessage(`{"amount":"100"}`), EligibilityTerms: json.RawMessage(`{"category":"washer"}`), StackingTerms: json.RawMessage(`{"status":"unknown"}`), Evidence: evidence, Reviewer: "reviewer", ReviewNote: "reviewed", ReviewExpiresAt: now.Add(7 * 24 * time.Hour), Applicability: &StructuredApplicability{CountryCode: "US", PostalCodes: []string{"78732"}, Category: "washer", Fuel: "electricity", ProviderKey: "utility-a", ProviderRole: "electric_distributor"}}
	rev, err := ImportReviewed(ctx, pool, input)
	if err != nil {
		t.Fatal(err)
	}
	var noLocationHome, noLocationAppliance uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'No location','America/Chicago') RETURNING id`, owner).Scan(&noLocationHome); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Washer','washer','{"fuel":"electricity"}') RETURNING id`, noLocationHome).Scan(&noLocationAppliance); err != nil {
		t.Fatal(err)
	}
	store := PGOwnerStore{Pool: pool}
	if _, err = store.List(ctx, owner.String(), noLocationHome.String(), noLocationAppliance.String(), now); !errorsIs(err, ErrLocationRequired) {
		t.Fatalf("missing location: %v", err)
	}
	if _, err = store.List(ctx, foreign.String(), noLocationHome.String(), noLocationAppliance.String(), now); !errorsIs(err, ErrOwnerHomeNotFound) {
		t.Fatalf("foreign missing-location read: %v", err)
	}
	router := chi.NewRouter()
	svc := OwnerService{Store: store, Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return owner.String() }}
	svc.Routes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+noLocationHome.String()+"/programs?appliance_id="+noLocationAppliance.String(), nil))
	var body struct {
		Programs []OwnerProgram `json:"programs"`
		Reason   string         `json:"reason"`
		AsOf     string         `json:"as_of"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Reason != "location_required" || body.Programs == nil || len(body.Programs) != 0 || body.AsOf == "" {
		t.Fatalf("location response: %d %s", w.Code, w.Body.String())
	}
	if _, err = store.List(ctx, foreign.String(), home.String(), appliance.String(), now); !errorsIs(err, ErrOwnerHomeNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	got, err := store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RevisionID != rev.String() || got[0].Status != "unknown" || !slices.Contains(got[0].Reasons, "provider_unknown") {
		t.Fatalf("provider candidate asserted: %+v", got)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','78733','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got) != 0 {
		t.Fatalf("ZIP boundary: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,3,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=now()-interval '1 second' WHERE source_key='utility-a'`); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got) != 0 {
		t.Fatalf("expired review: %+v %v", got, err)
	}
}
func errorsIs(err, target error) bool { return err == target }

func TestStructuredApplicabilityRejectsAmbiguousScope(t *testing.T) {
	cases := []StructuredApplicability{
		{CountryCode: "US", PostalCodes: []string{"78732", "78732"}, Category: "washer", Fuel: "electricity"},
		{CountryCode: "US", PostalCodes: []string{"7873"}, Category: "washer", Fuel: "electricity"},
		{CountryCode: "US", PostalCodes: []string{"78732"}, Category: "washer", Fuel: "electricity", ProviderKey: "utility-a"},
		{CountryCode: "CA", PostalCodes: []string{"78732"}, Category: "washer", Fuel: "electricity"},
	}
	for _, c := range cases {
		if validateStructured(&c) == nil {
			t.Fatalf("accepted ambiguous scope: %+v", c)
		}
	}
}
