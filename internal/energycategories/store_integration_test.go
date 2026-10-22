package energycategories

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCategoryRegistryAndETag(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "energycategories_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	store := PGStore{Pool: pool}
	initial, err := store.List(ctx)
	if err != nil || initial.Version < 1 || len(initial.Groups) != 10 {
		t.Fatalf("seed list: %+v %v", initial, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{"Boiler": "boiler", "boilers": "boiler", "furnace": "hvac", "heat_pump": "hvac"} {
		if got, err := Ensure(ctx, tx, raw); err != nil || got != want {
			t.Fatalf("Ensure(%q) = %q %v, want %q", raw, got, err, want)
		}
	}
	var label, group string
	if err = tx.QueryRow(ctx, `SELECT label,group_key FROM energy_categories WHERE key='boiler' AND origin='seed'`).Scan(&label, &group); err != nil || label != "Boiler" || group != "heating_cooling" {
		t.Fatalf("boiler seed row: %q %q %v", label, group, err)
	}
	key, err := Ensure(ctx, tx, "Kegerator")
	if err != nil || key != "kegerator" {
		t.Fatalf("unknown key: %q %v", key, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx)
	if err != nil || list.Version <= initial.Version {
		t.Fatalf("version did not advance: %d %v", list.Version, err)
	}
	found := false
	for _, g := range list.Groups {
		for _, item := range g.Types {
			if item.Key == "kegerator" && g.Key == "other" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("discovered category missing from picker")
	}
	exported, version, err := store.Export(ctx)
	if err != nil || version != list.Version {
		t.Fatalf("export: %d %v", version, err)
	}
	var doc map[string]any
	if err = json.Unmarshal(exported, &doc); err != nil || doc["version"] != "1.2.0" || doc["seed_version"] != nil {
		t.Fatalf("export shape: %v %v", doc["version"], err)
	}
	r := chi.NewRouter()
	auth := func(next http.Handler) http.Handler { return next }
	(Service{Store: store, Auth: auth}).Routes(r)
	first := httptest.NewRecorder()
	r.ServeHTTP(first, httptest.NewRequest("GET", "/energy-categories", nil))
	if first.Code != 200 || first.Header().Get("ETag") == "" {
		t.Fatalf("list HTTP: %d", first.Code)
	}
	replay := httptest.NewRequest("GET", "/energy-categories", nil)
	replay.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	r.ServeHTTP(second, replay)
	if second.Code != 304 || second.Body.Len() != 0 {
		t.Fatalf("conditional GET: %d %q", second.Code, second.Body.String())
	}
}
