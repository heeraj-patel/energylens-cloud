package energyrate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func migratedRatePool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "rates_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connCfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func TestStoredRatesServeTheRouteAndGas(t *testing.T) {
	ctx, pool := migratedRatePool(t)
	fetched := time.Date(2026, time.October, 1, 0, 30, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO energy_rate_cache(region,rate_usd_per_kwh,period,fetched_at) VALUES('TX',0.1588,'2026-07-01',$1),('US',0.1831,'2026-07-01',$1)`, fetched); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gas_rate_cache(region,rate_usd_per_therm,period,fetched_at) VALUES('TX',2.1194,'2026-07-01',$1),('US',1.4807,'2026-06-01',$1)`, fetched); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPgCache(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	gasCache, err := NewPgGasCache(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	now := fetched.Add(60 * 24 * time.Hour)
	service := NewService(Options{Cache: cache, GasCache: gasCache, Now: func() time.Time { return now }})
	router := chi.NewRouter()
	service.Routes(router)

	for query, want := range map[string]string{
		"country=US&postal_code=78732": `{"rate_usd_per_kwh":0.1588,"source":"state","source_label":"Texas average (EIA)","as_of":"2026-07-01","region":"TX","fetched_at":"2026-10-01T00:30:00Z","is_stale":false,"is_static":false}`,
		"country=US&postal_code=00601": `{"rate_usd_per_kwh":0.1831,"source":"national","source_label":"U.S. average (EIA)","as_of":"2026-07-01","region":null,"fetched_at":"2026-10-01T00:30:00Z","is_stale":false,"is_static":false}`,
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/energy/rates?"+query, nil))
		if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != want {
			t.Fatalf("%s: %d %s; want %s", query, recorder.Code, recorder.Body.String(), want)
		}
	}

	gas, err := service.ResolveGas(ctx, "78732")
	if err != nil || gas.Basis != GasBasisState || gas.USDPerTherm != 2.1194 || gas.AsOf != "2026-07-01" || gas.IsStale {
		t.Fatalf("state gas = %+v err=%v", gas, err)
	}
	gas, err = service.ResolveGas(ctx, "")
	if err != nil || gas.Basis != GasBasisNational || gas.USDPerTherm != 1.4807 || gas.AsOf != "2026-06-01" {
		t.Fatalf("national gas = %+v err=%v", gas, err)
	}
	if _, err := service.ResolveGas(ctx, "99501"); err != nil {
		t.Fatalf("AK falls back to US: %v", err)
	}
}
