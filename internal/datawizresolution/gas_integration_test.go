package datawizresolution

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFactlessGasDryerNeverGetsElectricTypical(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawizresolution_gas_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, product := uuid.NewString(), uuid.NewString(), uuid.NewString()
	gas, electric := uuid.NewString(), uuid.NewString()
	typical := `{"schema_version":"datawiz.energy-resolution.v1","appliance_version":1,"status":"resolved","device_kind":"real_non_metering","source":{"tier":"category_typical"},"values":{"kwh_per_day":3},"waste_floor":{"usd_per_day":0.02}}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Samsung','DVG45T6000V','natural_gas','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES($1,$2,'Gas dryer','dryer','{"brand":"Samsung","full_model":"DVG45T6000V","fuel":"natural_gas","match_quality":"confirmed_exact"}',$3)`, []any{gas, home, product}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dryer','dryer','{"brand":"Acme","full_model":"E1","fuel":"electricity","match_quality":"unresolved"}')`, []any{electric, home}},
		{`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$3,$4,now()),($2,$3,$4,now())`, []any{"cloud:" + gas, "cloud:" + electric, make([]byte, 32), typical}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	store := PGStore{Pool: pool}
	if body, e := store.ForOwner(ctx, owner, home, gas); !errors.Is(e, ErrNotFound) {
		t.Fatalf("gas dryer got an electric typical: %s %v", body, e)
	}
	body, err := store.ForOwner(ctx, owner, home, electric)
	if err != nil || !strings.Contains(string(body), `"kwh_per_day": 3`) || !strings.Contains(string(body), "waste_floor") {
		t.Fatalf("electric dryer changed: %s %v", body, err)
	}
}
