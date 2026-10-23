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
	evidence["application_to"] = "section 3"
	input := ReviewedImport{SourceKey: "utility-a", SourceURL: "https://utility.example/program", SourceBytes: []byte("source terms; fixture benefit text"), ContentType: "text/html", SnapshotRef: "test-source", ParserVersion: "v1", AdministratorKey: "utility-a", ProgramKey: "washer", GeographyKey: "78732", AdministratorName: "Utility A", AdministratorKind: "utility", Status: "open", Title: "Washer rebate", BenefitKind: "rebate", BenefitDisplay: "fixture benefit text", BenefitTerms: json.RawMessage(`{"amount":"100"}`), EligibilityTerms: json.RawMessage(`{"category":"washer"}`), StackingTerms: json.RawMessage(`{"status":"unknown"}`), Evidence: evidence, Reviewer: "reviewer", ReviewNote: "reviewed", ReviewExpiresAt: now.Add(7 * 24 * time.Hour), Applicability: &StructuredApplicability{CountryCode: "US", PostalCodes: []string{"78732"}, Category: "washer", Fuel: "electricity", ProviderKey: "utility-a", ProviderRole: "electric_distributor"}}
	zone, _ := time.LoadLocation("America/Chicago")
	input.Dates = map[string]string{"application_to": now.In(zone).Format("2006-01-02")}
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
	if _, err = store.ListHome(ctx, owner.String(), noLocationHome.String(), now); !errorsIs(err, ErrLocationRequired) {
		t.Fatalf("missing home location: %v", err)
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
	if len(got.Programs) != 1 || got.Programs[0].RevisionID != rev.String() || got.Programs[0].Status != "unknown" || !slices.Contains(got.Programs[0].Reasons, "provider_unknown") {
		t.Fatalf("provider candidate asserted: %+v", got)
	}
	if got.Programs[0].AdministratorKind != "utility" || got.Programs[0].BenefitDisplay != input.BenefitDisplay {
		t.Fatalf("reviewed administrator kind or benefit missing from owner feed: %+v", got.Programs[0])
	}
	var terms map[string]any
	if got.Programs[0].AvailabilityStatus != "open" || json.Unmarshal(got.Programs[0].BenefitTerms, &terms) != nil || terms["amount"] != "100" {
		t.Fatalf("reviewed benefit terms or availability missing: %+v", got.Programs[0])
	}
	if len(got.Programs[0].EligibilityTerms) == 0 || len(got.Programs[0].StackingTerms) == 0 {
		t.Fatalf("reviewed conditions missing: %+v", got.Programs[0])
	}
	deadline := applicationDeadline(now.In(zone), zone)
	if got.Programs[0].AvailabilityExpiresAt == nil || *got.Programs[0].AvailabilityExpiresAt != deadline.UTC().Format(time.RFC3339) || got.Programs[0].ReviewExpiresAt == *got.Programs[0].AvailabilityExpiresAt {
		t.Fatalf("application deadline or source review expiry lost: %+v", got.Programs[0])
	}
	after, err := store.List(ctx, owner.String(), home.String(), appliance.String(), deadline)
	if err != nil || len(after.Programs) != 1 || !slices.Contains(after.Programs[0].Reasons, "expired") {
		t.Fatalf("application-to boundary assessment: %+v %v", after, err)
	}
	// An ineligible assessment remains represented while its source revision
	// and availability stay attached to the card data.
	input.Status = "closed"
	input.SourceBytes = []byte("source terms; fixture benefit text; closed availability")
	if _, err = ImportReviewed(ctx, pool, input); err != nil {
		t.Fatal(err)
	}
	closed, err := store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(closed.Programs) != 1 || closed.Programs[0].Status != "ineligible" || closed.Programs[0].AvailabilityStatus != "closed" {
		t.Fatalf("closed matching program must remain in owner feed: %+v %v", closed, err)
	}
	input.Status = "open"
	input.SourceBytes = []byte("source terms; fixture benefit text; restored availability")
	rev, err = ImportReviewed(ctx, pool, input)
	if err != nil {
		t.Fatal(err)
	}
	// A reviewed, fully modeled non-utility offer can be a potential match.
	// It remains only a potential match and never an eligibility approval.
	input.ProgramKey = "washer-general"
	input.BenefitTerms = json.RawMessage(`{"amount":"200","currency":"USD"}`)
	input.Applicability = &StructuredApplicability{CountryCode: "US", PostalCodes: []string{"78732"}, Category: "washer", Fuel: "electricity"}
	input.SourceBytes = []byte("separately recorded source terms; fixture benefit text")
	if _, err = ImportReviewed(ctx, pool, input); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got.Programs) != 2 {
		t.Fatalf("reviewed household assessment: %+v %v", got, err)
	}
	firstSaving, comparable := fixedOfferSaving(got.Programs[0])
	if !comparable || firstSaving.RatString() != "200" {
		t.Fatalf("larger fixed rebate should rank first: %+v", got.Programs)
	}
	homeGot, err := store.ListHome(ctx, owner.String(), home.String(), now)
	if err != nil || len(homeGot.Programs) != 2 {
		t.Fatalf("home read must include matching appliance offers: %+v %v", homeGot, err)
	}
	var potential bool
	for _, item := range got.Programs {
		if item.Status == "potentially_eligible" && slices.Contains(item.Reasons, "terms_require_confirmation") {
			potential = true
		}
	}
	if !potential {
		t.Fatalf("fully modeled potential match missing: %+v", got.Programs)
	}
	if got.Coverage != nil {
		t.Fatalf("invented discovery coverage: %+v", got.Coverage)
	}
	var oldRun, latestRun, otherCategoryRun uuid.UUID
	for _, run := range []struct {
		zip, category, state string
		id                   *uuid.UUID
	}{
		{"78732", "", "complete", &oldRun},
		{"78732", "washer", "partial", &latestRun},
		{"78732", "dryer", "complete", &otherCategoryRun},
	} {
		if err = pool.QueryRow(ctx, `INSERT INTO program_discovery_runs(country_code,postal_code,category,state,limitations,finished_at)
			VALUES('US',$1,$2,$3,'["zip_provider_discovery_incomplete"]',now()) RETURNING id`, run.zip, nullableText(run.category), run.state).Scan(run.id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO program_discovery_coverage(run_id,scope_kind,scope_key,scope_name,provider_role,status,checked_at)
		VALUES($1,'utility','utility-a','Utility A','electric_distributor','checked',now()),
		($1,'county','travis','Travis County',NULL,'pending',NULL)`, latestRun); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || got.Coverage == nil || got.Coverage.RunID != latestRun.String() || got.Coverage.State != "partial" || len(got.Coverage.Scopes) != 2 || got.Coverage.Scopes[1].Status != "checked" {
		t.Fatalf("latest matching discovery run: %+v %v", got.Coverage, err)
	}
	var utility *OwnerProgram
	for i := range got.Programs {
		if got.Programs[i].RevisionID == rev.String() {
			utility = &got.Programs[i]
		}
	}
	if utility == nil || utility.Status != "unknown" || !slices.Contains(utility.Reasons, "provider_unconfirmed") {
		t.Fatalf("ZIP provider candidate must remain unconfirmed: %+v", got.Programs)
	}
	// A candidate for a different service role cannot satisfy this requirement.
	if _, err = pool.Exec(ctx, `UPDATE program_discovery_coverage SET provider_role='gas_distributor' WHERE run_id=$1 AND scope_key='utility-a'`, latestRun); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range got.Programs {
		if item.RevisionID == rev.String() && (item.Status != "unknown" || !slices.Contains(item.Reasons, "provider_unknown")) {
			t.Fatalf("mismatched provider role asserted: %+v", item)
		}
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home.String()+"/programs?appliance_id="+appliance.String(), nil))
	var page struct {
		Coverage *OwnerCoverage `json:"coverage"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Coverage == nil || page.Coverage.RunID != latestRun.String() {
		t.Fatalf("owner coverage response: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Programs []OwnerProgram `json:"programs"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Programs) != 2 || response.Programs[0].AvailabilityStatus != "open" {
		t.Fatalf("availability must be independent of discovery coverage: %s", w.Body.String())
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','78733','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got.Programs) != 0 || got.Coverage != nil {
		t.Fatalf("ZIP boundary: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,3,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=now()-interval '1 second' WHERE source_key='utility-a'`); err != nil {
		t.Fatal(err)
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got.Programs) != 0 {
		t.Fatalf("expired review: %+v %v", got, err)
	}
	// A source-cited national household program needs no appliance and no
	// fabricated ZIP enumeration. Income and other conditions stay unknown.
	_, err = pool.Exec(ctx, `INSERT INTO program_sources(source_key,source_kind,authority_name,administrator_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled) VALUES('federal-a','government','Federal A','Federal A','https://federal.example/program',ARRAY['federal.example'],'html','https://federal.example/terms',$1,$2,'reviewer','v1','v1','{}','[]',interval '7 days',20,1000000,true)`, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	input.SourceKey = "federal-a"
	input.SourceURL = "https://federal.example/program"
	input.SourceBytes = []byte("national household assistance fixture benefit and conditions")
	input.AdministratorKey = "federal-a"
	input.AdministratorName = "Federal A"
	input.AdministratorKind = "federal"
	input.ProgramKey = "home-assistance"
	input.GeographyKey = "US"
	input.Title = "Home assistance"
	input.Evidence["applicability.geography_scope"] = "section 2"
	input.Applicability = &StructuredApplicability{CountryCode: "US", GeographyScope: "country", Category: "home", Fuel: "any", RequiresIncome: true, AdditionalConditions: true}
	if _, err = ImportReviewed(ctx, pool, input); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListHome(ctx, owner.String(), home.String(), now)
	if err != nil || len(got.Programs) != 1 || got.Programs[0].AdministratorKind != "federal" || got.Programs[0].Status != "unknown" || !slices.Contains(got.Programs[0].Reasons, "income_unknown") || !slices.Contains(got.Programs[0].Reasons, "additional_conditions_unknown") {
		t.Fatalf("national household offer: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,4,'CA','H0H0H0','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListHome(ctx, owner.String(), home.String(), now)
	if err != nil || len(got.Programs) != 0 {
		t.Fatalf("national country boundary: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,5,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListHome(ctx, foreign.String(), home.String(), now); !errorsIs(err, ErrOwnerHomeNotFound) {
		t.Fatalf("foreign home read: %v", err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home.String()+"/programs", nil))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Programs) != 1 || response.Programs[0].SourceURL != input.SourceURL {
		t.Fatalf("home HTTP response: %d %s", w.Code, w.Body.String())
	}
	got, err = store.List(ctx, owner.String(), home.String(), appliance.String(), now)
	if err != nil || len(got.Programs) != 0 {
		t.Fatalf("household program leaked into appliance read: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE program_sources SET terms_review_expires_at=now()-interval '1 second' WHERE source_key='federal-a'`); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListHome(ctx, owner.String(), home.String(), now)
	if err != nil || len(got.Programs) != 0 {
		t.Fatalf("expired national review: %+v %v", got, err)
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

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
