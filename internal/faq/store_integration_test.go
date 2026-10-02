package faq

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

func TestCurrentEstimateOwnerPeriodRevisionAndInvalidation(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "faq_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, appliance, run := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Device','refrigerator','{}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	now := time.Now().UTC()
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); err != nil || got != nil {
		t.Fatalf("missing current run: %+v %v", got, err)
	}
	if _, err := store.CurrentEstimate(ctx, other, home, nil, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner fallback: %v", err)
	}
	start := now.Format("2006-01") + "-01"
	end := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	result := `{"appliance_id":"` + appliance + `","kind":"estimated_operating_cost","period":{"start":"` + start + `","end":"` + end + `"},"cost":{"value":"1.00","currency":"USD"},"evidence_refs":[]}`
	_, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json) VALUES($1,$2,$3,1,$4,$5,'UTC','test',$6,'{}',$7)`, run, home, appliance, start, end, make([]byte, 32), result)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); err != nil || got == nil || len(got.Result) == 0 {
		t.Fatalf("current run: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2 WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	staleRun := uuid.NewString()
	staleHash := make([]byte, 32)
	staleHash[0] = 1
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json) VALUES($1,$2,$3,1,$4,$5,'UTC','test',$6,'{}',$7)`, staleRun, home, appliance, start, end, staleHash, result); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); err != nil || got != nil {
		t.Fatalf("stale appliance revision: %+v %v", got, err)
	}
	currentRun := uuid.NewString()
	currentHash := make([]byte, 32)
	currentHash[0] = 2
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json) VALUES($1,$2,$3,2,$4,$5,'UTC','test',$6,'{}',$7)`, currentRun, home, appliance, start, end, currentHash, result); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); err != nil || got == nil {
		t.Fatalf("new appliance revision: %+v %v", got, err)
	}
	nextMonth := time.Date(now.Year(), now.Month()+1, 15, 12, 0, 0, 0, time.UTC)
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, nextMonth); err != nil || got != nil {
		t.Fatalf("prior month run: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_run_invalidations(run_id,reason) VALUES($1,'dependency_changed')`, currentRun); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); err != nil || got != nil {
		t.Fatalf("invalidated run: %+v %v", got, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET owner_user_id=$1 WHERE id=$2`, other, home); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentEstimate(ctx, owner, home, &appliance, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("former owner: %v", err)
	}
}

func TestCurrentWeatherOwnerLocationAndCompletedDays(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "faq_weather_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','America/Chicago')`, home, owner); err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if rows, rev, err := store.CurrentWeather(ctx, owner, home, now); err != nil || len(rows) != 0 || rev != 0 {
		t.Fatalf("missing location: %d %d %v", len(rows), rev, err)
	}
	if _, _, err := store.CurrentWeather(ctx, other, home, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	for ago := 14; ago >= 0; ago-- {
		day := now.AddDate(0, 0, -ago).Format("2006-01-02")
		if _, err = pool.Exec(ctx, `INSERT INTO weather_observations(provider,cell_key,local_date,timezone,mean_temperature_c,source_revision,retrieved_at,expires_at) VALUES('test','US:787:America/Chicago',$1,'America/Chicago',20,'source-1',$2,$3)`, day, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rows, rev, err := store.CurrentWeather(ctx, owner, home, now)
	if err != nil || len(rows) != 14 || rev != 1 {
		t.Fatalf("completed days: %d %d %v", len(rows), rev, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','60601','postal_code','user_confirmed')`, home); err != nil {
		t.Fatal(err)
	}
	rows, rev, err = store.CurrentWeather(ctx, owner, home, now)
	if err != nil || len(rows) != 0 || rev != 2 {
		t.Fatalf("changed location: %d %d %v", len(rows), rev, err)
	}
}
