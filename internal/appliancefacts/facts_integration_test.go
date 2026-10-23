package appliancefacts

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/datawizresolution"
	"energylens.local/cloud/internal/estimation"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/questions"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFactsAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "appliancefacts_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, stranger, home, other := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	gas, electricID := uuid.NewString(), uuid.NewString()
	record := `{"schema_version":"datawiz.energy-resolution.v1","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","source":{"tier":"energy_star"},"values":{"kwh_per_day":1}}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, stranger}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$3,'Home','America/Chicago'),($2,$4,'Other','America/Chicago')`, []any{home, other, owner, stranger}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Furnace','furnace','{"brand":"Acme","full_model":"G1","fuel":"natural_gas","match_quality":"unresolved"}')`, []any{gas, home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Washer','washer','{"brand":"Acme","full_model":"W1","fuel":"electricity","match_quality":"unresolved"}')`, []any{electricID, home}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, []any{"cloud:" + electricID, make([]byte, 32), record}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	svc := Service{
		Composer: Composer{
			Appliances:      inventory.PGStore{Pool: pool},
			Resolutions:     datawizresolution.PGStore{Pool: pool},
			Estimates:       estimation.PGRunStore{Pool: pool},
			Programs:        programs.PGOwnerStore{Pool: pool},
			Recommendations: recommendation.PGStore{Pool: pool},
			Answers:         questions.PGStore{Pool: pool},
			Efficiency:      householdenergy.Service{Pool: pool},
			Revisions:       PGRevisions{Pool: pool},
		},
		Auth: func(next http.Handler) http.Handler { return next },
	}
	asOwner, asStranger := svc, svc
	asOwner.UserID = func(*http.Request) string { return owner }
	asStranger.UserID = func(*http.Request) string { return stranger }
	route := func(s Service) http.Handler { r := chi.NewRouter(); s.Routes(r); return r }
	path := func(id string) string { return "/homes/" + home + "/appliances/" + id + "/facts" }

	rec := get(route(asOwner), path(electricID))
	body := decode(t, rec)
	if body["identity"]["status"] != "ok" || body["catalog_facts"]["status"] != "ok" || body["efficiency"]["status"] == "error" {
		t.Fatalf("electric: %s", rec.Body)
	}
	gasBody := decode(t, get(route(asOwner), path(gas)+"?capability=gas_appliance_recommendations_v1"))
	if gasBody["catalog_facts"]["status"] != "empty" || gasBody["estimate"]["status"] != "unavailable" {
		t.Fatalf("gas: %v", gasBody)
	}
	if got := get(route(asStranger), path(electricID)); got.Code != 404 {
		t.Fatalf("stranger got %d", got.Code)
	}
	tag := rec.Header().Get("ETag")
	if got := get(route(asOwner), path(electricID), "If-None-Match", tag); got.Code != 304 {
		t.Fatalf("matching ETag got %d", got.Code)
	}
	if _, err = pool.Exec(ctx, `UPDATE datawiz_energy_resolutions SET resolved_at=resolved_at+interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got := get(route(asOwner), path(electricID), "If-None-Match", tag); got.Code != 200 {
		t.Fatalf("changed resolution still got %d", got.Code)
	}
}
