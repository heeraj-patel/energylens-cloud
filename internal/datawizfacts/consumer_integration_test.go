package datawizfacts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/energyrate"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/ratedinputapi"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/internal/tariff"
	"energylens.local/cloud/internal/usagefactapi"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentResearchReachesExistingReaders(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawiz_consumer_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var owner, home uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Home','UTC') RETURNING id`, owner).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	product := uuid.New()
	productJSON := `{"id":"` + product.String() + `","category":"heat_pump","brand":"ACIQ","full_model":"M-1","variant":null,"region":null,"fuel":"electricity","match_quality":"confirmed_exact"}`
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'heat_pump','ACIQ','M-1','electricity','confirmed_exact',$2)`, product, productJSON); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(home_id,name,category,identity,catalog_product_id) VALUES($1,'Heat pump','heat_pump',$2,$3)`, home, `{"fuel":"electricity","brand":"ACIQ","full_model":"M-1","match_quality":"confirmed_exact"}`, product); err != nil {
		t.Fatal(err)
	}
	source := `{"adapter":"fixture_official","url":"https://example.org/source","retrieved_at":"2026-10-12T00:00:00Z"}`
	seed := func(route, subject, field, record, value string) {
		t.Helper()
		run, observation := uuid.New(), uuid.New()
		if _, e := pool.Exec(ctx, `INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,$2,'{}','realtime','complete')`, run, route); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.Exec(ctx, `INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'approved','2026-10-12T00:00:00Z')`, observation, run, route, subject, field, record, value, source); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.Exec(ctx, `INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES($1,$2,$3,$4,$5)`, route, subject, field, record, observation); e != nil {
			t.Fatal(e)
		}
	}
	device := datawizfacts.DeviceSubject("ACIQ", "M-1")
	seed("device", device, "device_identity", "oem:aciq:m1", `{"manufacturer_key":"aciq","model_key":"m1","relationship":"exact_model_on_official_oem_page"}`)
	seed("device", device, "energy_rating", "oem:aciq:m1:annual", `{"metric":"annual_kwh","amount":"700","unit":"kWh/year","model_scope":"exact"}`)
	seed("device", device, "energy_rating", "oem:aciq:m1:watts", `{"metric":"rated_input","amount":"1200","unit":"W","model_scope":"exact"}`)
	seed("device", device, "energy_rating", "oem:aciq:m1:seer", `{"metric":"SEER2","amount":"17.8","unit":"Btu/Wh","model_scope":"exact"}`)
	seed("device", datawizfacts.DeviceSubject("ACIQ", "N-2"), "energy_rating", "manual:"+uuid.NewString(), `{"metric":"rated_watts","amount":"1200","unit":"W","test_context":"nameplate"}`)
	seed("zip", "zip:78732", "jurisdiction", "census:county", `{"name":"Travis County","geography_type":"county","state":"Texas"}`)
	seed("zip", "zip:78732", "provider_candidate", "oedi:1015", `{"name":"Austin Energy","service_type":"electric","provider_id":"1015","candidate_status":"likely_zip_assignment_unconfirmed"}`)
	seed("zip", "zip:78732", "rate", "oedi:1015:average", `{"amount":"0.12","unit":"USD/kWh","rate_basis":"utility_average","candidate_status":"average_not_household_tariff"}`)
	seed("zip", "zip:78732", "government_offer", "local:one", `{"program_name":"Example offer","eligibility_status":"unknown","availability_status":"unknown","terms_excerpt":"Example terms"}`)
	seed("savings", datawizfacts.SavingsSubject("ACIQ", "heat_pump"), "manufacturer_offer", "oem:offer", `{"manufacturer":"ACIQ","device_type":"heat_pump","eligibility_status":"unknown","amount":null,"expires_at":null}`)
	seed("efficiency", "efficiency:night", "efficiency_tip", "night:one", `{"context":"night","applicable_categories":["hvac"],"excluded_categories":[],"prerequisites":["programmable thermostat"],"savings_type":"energy_reduction","numeric_savings":null}`)
	products, err := (catalog.PGStore{Pool: pool}).Search(ctx, catalog.Search{Brand: "aciq", Model: "m-1", Fuel: "electricity"})
	if err != nil || len(products) != 1 {
		t.Fatalf("catalog search: %d %v", len(products), err)
	}
	var catalogBody struct {
		Research []datawizfacts.Fact `json:"research_facts"`
	}
	if err = json.Unmarshal(products[0], &catalogBody); err != nil || len(catalogBody.Research) != 4 {
		t.Fatalf("catalog research: %v %+v", err, catalogBody)
	}
	searchOnly, err := (catalog.PGStore{Pool: pool}).Search(ctx, catalog.Search{Brand: "ACIQ", Model: "N-2", Fuel: "electricity"})
	if err != nil || len(searchOnly) != 0 {
		t.Fatalf("source-only catalog products: %d %v", len(searchOnly), err)
	}
	searchResearch, err := (catalog.PGStore{Pool: pool}).SearchResearch(ctx, catalog.Search{Brand: "ACIQ", Model: "N-2", Fuel: "electricity"})
	if err != nil || len(searchResearch) != 1 {
		t.Fatalf("source-only catalog research: %d %v", len(searchResearch), err)
	}
	fuelAgnosticResearch, err := (catalog.PGStore{Pool: pool}).SearchResearch(ctx, catalog.Search{Brand: "ACIQ", Model: "N-2", Fuel: "natural_gas"})
	if err != nil || len(fuelAgnosticResearch) != 1 {
		t.Fatalf("model-subject research hidden by fuel: %d %v", len(fuelAgnosticResearch), err)
	}
	manualRated, err := (ratedinputapi.PGStore{Pool: pool}).Research(ctx, ratedinputapi.Scope{Category: "heat_pump", Manufacturer: "ACIQ", Model: "N-2"})
	if err != nil || len(manualRated) != 1 || !strings.HasPrefix(manualRated[0].RecordKey, "manual:") {
		t.Fatalf("approved manual rated watts: %+v %v", manualRated, err)
	}
	router := chi.NewRouter()
	(catalog.Service{Store: catalog.PGStore{Pool: pool}, Auth: func(next http.Handler) http.Handler { return next }}).Routes(router)
	req := httptest.NewRequest(http.MethodPost, "/catalog/model-search", strings.NewReader(`{"brand":"ACIQ","model":"N-2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	var searchBody struct {
		Products []json.RawMessage   `json:"products"`
		Research []datawizfacts.Fact `json:"research_facts"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &searchBody) != nil || len(searchBody.Products) != 0 || len(searchBody.Research) != 1 {
		t.Fatalf("source-only catalog API: %d %s", response.Code, response.Body.String())
	}
	usage, err := (usagefactapi.PGStore{Pool: pool}).Research(ctx, usagefactapi.Scope{Manufacturer: "ACIQ", Model: "M-1", Fuel: "electricity"})
	if err != nil || len(usage) != 1 {
		t.Fatalf("compatible usage evidence: %d %v", len(usage), err)
	}
	usageRouter := chi.NewRouter()
	(usagefactapi.Service{Store: usagefactapi.PGStore{Pool: pool}, Auth: func(next http.Handler) http.Handler { return next }}).Routes(usageRouter)
	usageResponse := httptest.NewRecorder()
	usageRouter.ServeHTTP(usageResponse, httptest.NewRequest(http.MethodGet, "/usage-facts?category=heat_pump&manufacturer=ACIQ&model=M-1&fuel=electricity&variant=standard", nil))
	var usageBody struct {
		Facts    []json.RawMessage   `json:"facts"`
		Research []datawizfacts.Fact `json:"research_facts"`
	}
	if usageResponse.Code != 200 || json.Unmarshal(usageResponse.Body.Bytes(), &usageBody) != nil || len(usageBody.Facts) != 0 || len(usageBody.Research) != 1 {
		t.Fatalf("source-only usage API: %d %s", usageResponse.Code, usageResponse.Body.String())
	}
	rated, err := (ratedinputapi.PGStore{Pool: pool}).Research(ctx, ratedinputapi.Scope{Manufacturer: "ACIQ", Model: "M-1"})
	if err != nil || len(rated) != 1 {
		t.Fatalf("rated watts: %d %v", len(rated), err)
	}
	resolver := programs.OEDIZIPResolver{DB: pool}
	zip, err := resolver.ResolveZIPDetailed(ctx, "US", "78732")
	if err != nil || len(zip.Scopes) != 2 {
		t.Fatalf("ZIP scopes: %+v %v", zip, err)
	}
	discovery, err := (programs.DiscoveryRunner{DB: pool, Resolver: resolver}).Run(ctx, programs.DiscoveryRequest{CountryCode: "US", PostalCode: "78732", Category: "heat_pump"})
	if err != nil || discovery.RunID == uuid.Nil {
		t.Fatalf("Datawiz ZIP discovery: %+v %v", discovery, err)
	}
	var discoveryProvenance []byte
	if err = pool.QueryRow(ctx, `SELECT resolution_provenance FROM program_discovery_runs WHERE id=$1`, discovery.RunID).Scan(&discoveryProvenance); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(discoveryProvenance), `"datawiz_offer_candidate_count": 1`) && !strings.Contains(string(discoveryProvenance), `"datawiz_offer_candidate_count":1`) {
		t.Fatalf("discovery offers missing: %s", discoveryProvenance)
	}
	rate := energyrate.NewService(energyrate.Options{Research: &datawizfacts.Store{DB: pool}, Static: energyrate.StaticRate{USDPerKWh: 0.15, AsOf: time.Now()}})
	resolved, err := rate.Resolve(ctx, "US", "78732")
	if err != nil || len(resolved.ResearchFacts) != 3 || resolved.RateUSDPerKWh == 0.12 {
		t.Fatalf("regional rate / candidate separation: %+v %v", resolved, err)
	}
	tariffs, err := (tariff.PGStore{Pool: pool}).List(ctx, owner.String(), home.String())
	if err != nil || len(tariffs) != 0 {
		t.Fatalf("selected tariffs: %d %v", len(tariffs), err)
	}
	tariffResearch, err := (tariff.PGStore{Pool: pool}).Research(ctx, owner.String(), home.String())
	if err != nil || len(tariffResearch) != 2 {
		t.Fatalf("tariff research: %d %v", len(tariffResearch), err)
	}
	ownerResult, err := (programs.PGOwnerStore{Pool: pool}).ListHome(ctx, owner.String(), home.String(), time.Now())
	if err != nil || len(ownerResult.ResearchFacts) != 2 || len(ownerResult.Programs) != 0 {
		t.Fatalf("owner offers: %+v %v", ownerResult, err)
	}
	appliances, err := (recommendation.PGStore{Pool: pool}).Current(ctx, owner.String(), home.String(), time.Now())
	if err != nil || len(appliances) != 1 {
		t.Fatalf("recommendation snapshot: %d %v", len(appliances), err)
	}
	actions := recommendation.Evaluate(appliances[0])
	if len(actions) != 1 || len(actions[0].ResearchEvidence) != 1 || actions[0].SavingsAmount != nil || actions[0].State != "candidate" {
		t.Fatalf("recommendation research: %+v", actions)
	}
	for _, contextName := range []string{"away", "peak_usage", "habits", "appliance_type", "upgrades", "maintenance"} {
		seed("efficiency", "efficiency:"+contextName, "efficiency_tip", contextName+":one", `{"context":"`+contextName+`","applicable_categories":["hvac"],"excluded_categories":[],"prerequisites":[],"savings_type":"energy_reduction","numeric_savings":null}`)
	}
	var emptyHome uuid.UUID
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Empty','UTC') RETURNING id`, owner).Scan(&emptyHome); err != nil {
		t.Fatal(err)
	}
	recommendationRouter := chi.NewRouter()
	(recommendation.Service{Store: recommendation.PGStore{Pool: pool}, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner.String() }}).Routes(recommendationRouter)
	readRecommendations := func() (int, int, int) {
		t.Helper()
		response := httptest.NewRecorder()
		recommendationRouter.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/homes/"+emptyHome.String()+"/recommendations", nil))
		var body struct {
			Actions  []recommendation.Action `json:"actions"`
			Research []datawizfacts.Fact     `json:"research_facts"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return response.Code, len(body.Actions), len(body.Research)
	}
	if status, actionCount, researchCount := readRecommendations(); status != 200 || actionCount != 0 || researchCount != 7 {
		t.Fatalf("empty-home recommendation research: status=%d actions=%d research=%d", status, actionCount, researchCount)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'HVAC','hvac','{"fuel":"electricity","match_quality":"unresolved"}')`, emptyHome); err != nil {
		t.Fatal(err)
	}
	if status, actionCount, researchCount := readRecommendations(); status != 200 || actionCount != 1 || researchCount != 7 {
		t.Fatalf("HVAC without legacy rule: status=%d actions=%d research=%d", status, actionCount, researchCount)
	}
}
