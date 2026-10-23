package recommendation

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

func TestPeakDemandStoreReadsTipsAndHomeUtility(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_peak_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, bare := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	run := uuid.NewString()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Austin','America/Chicago'),($3,$2,'Bare','UTC')`, []any{home, owner, bare}},
		{`INSERT INTO home_electric_utility(home_id,revision,eiaid,chosen_by) VALUES($1,1,'14328','zip_single'),($1,2,'1015','user')`, []any{home}},
		{`INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,'efficiency','{}','realtime','complete')`, []any{run}},
	}
	for _, c := range []string{"habits", "peak_demand_day"} {
		observation := uuid.NewString()
		value := `{"context":"` + c + `","applicable_categories":["home"],"applicable_provider_ids":["1015"]}`
		stmts = append(stmts, struct {
			q    string
			args []any
		}{`INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at) VALUES($1,$2,'efficiency',$3,'efficiency_tip',$4,$5,'{"adapter":"fixture_official","url":"https://austinenergy.com/x","retrieved_at":"2026-10-12T00:00:00Z"}','approved','2026-10-12T00:00:00Z')`,
			[]any{observation, run, "efficiency:" + c, c, value}}, struct {
			q    string
			args []any
		}{`INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES('efficiency',$1,'efficiency_tip',$2,$3)`, []any{"efficiency:" + c, c, observation}})
	}
	for _, stmt := range stmts {
		if _, err = pool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	now := time.Date(2026, time.October, 1, 3, 0, 0, 0, time.UTC)
	h, err := store.PeakDemandHome(ctx, owner, home, now)
	if err != nil || h.EIAID != "1015" || h.Local.Month() != time.September || h.Local.Day() != 30 || !inPeakSeason(h.Local) {
		t.Fatalf("home %+v, %v", h, err)
	}
	if h, err = store.PeakDemandHome(ctx, owner, bare, now); err != nil || h.EIAID != "" {
		t.Fatalf("no utility: %+v, %v", h, err)
	}
	if _, err = store.PeakDemandHome(ctx, other, home, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	tips, err := store.PeakDemandTips(ctx)
	if err != nil || len(tips) != 1 || tips[0].RecordKey != "peak_demand_day" {
		t.Fatalf("peak tips: %+v, %v", tips, err)
	}
	legacy, err := store.Research(ctx)
	if err != nil || len(legacy) != 1 || legacy[0].RecordKey != "habits" {
		t.Fatalf("legacy research: %+v, %v", legacy, err)
	}
	if weatherTips, err := store.WeatherTips(ctx); err != nil || len(weatherTips) != 0 {
		t.Fatalf("weather tips: %+v, %v", weatherTips, err)
	}
	s := Service{Store: store}
	if _, page, err := s.withPeakDemand(ctx, owner, home, now, nil); err != nil || page == nil || page.SourceURL != "https://austinenergy.com/x" || len(page.HomeTips) != 1 {
		t.Fatalf("page %+v, %v", page, err)
	}
	if _, page, err := s.withPeakDemand(ctx, owner, home, now.Add(24*time.Hour), nil); err != nil || page != nil {
		t.Fatalf("october page %+v, %v", page, err)
	}
}
