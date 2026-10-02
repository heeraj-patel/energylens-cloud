package weather

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

func TestPGWeatherCoarseScopeAndImmutableEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "weather_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, owner, other); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, home, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	if _, _, err = store.HomeLocation(ctx, other, home); !errors.Is(err, ErrHomeNotFound) {
		t.Fatalf("cross-owner location: %v", err)
	}
	loc, supported, err := store.HomeLocation(ctx, owner, home)
	if err != nil || !supported || loc.CellKey != "US:787:America/Chicago" || loc.LocationRevision != 1 {
		t.Fatalf("coarse location: %+v %t %v", loc, supported, err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	obs, err := store.Save(ctx, Observation{Provider: "fake-weather", CellKey: loc.CellKey, LocalDate: "2026-09-30", Timezone: loc.Timezone, MeanTemperatureC: 23.5, SourceRevision: "source-1", RetrievedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
	if err != nil || obs.ID == "" {
		t.Fatalf("save: %+v %v", obs, err)
	}
	read, err := store.Latest(ctx, "fake-weather", loc.CellKey, "2026-09-30", loc.Timezone)
	if err != nil || read == nil || read.ID != obs.ID || read.SourceRevision != "source-1" {
		t.Fatalf("read: %+v %v", read, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE weather_observations SET mean_temperature_c=99 WHERE id=$1`, obs.ID); err == nil {
		t.Fatal("weather evidence was mutable")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','60601','postal_code','user_confirmed')`, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET version=2 WHERE id=$1`, home); err != nil {
		t.Fatal(err)
	}
	loc, supported, err = store.HomeLocation(ctx, owner, home)
	if err != nil || !supported || loc.CellKey != "US:606:America/Chicago" || loc.HomeVersion != 2 || loc.LocationRevision != 2 {
		t.Fatalf("changed location: %+v %t %v", loc, supported, err)
	}
	read, err = store.Latest(ctx, "fake-weather", loc.CellKey, "2026-09-30", loc.Timezone)
	if err != nil || read != nil {
		t.Fatalf("old cell reused: %+v %v", read, err)
	}
}
