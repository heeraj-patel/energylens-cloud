package householdenergy

import (
	"context"
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

func TestTOUDayReadsTheHomeTariffFromTheDatabase(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "tou_day_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, touHome, flatHome := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{touHome, flatHome} {
		if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','America/Los_Angeles')`, home, owner); err != nil {
			t.Fatal(err)
		}
		var revision string
		if err = pool.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
 VALUES($1,1,'electricity',0.30,'kWh','USD','user_tariff','bill',now()-interval '1 day',false,'2020-01-01T00:00:00Z') RETURNING id::text`, home).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if home != touHome {
			continue
		}
		for i, row := range [][]any{{"peak", "0.15", everyDay, "16:00", "21:00"}, {"night", "-0.05", everyDay, "00:00", "06:00"}} {
			if _, err = pool.Exec(ctx, `INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,$2,$3,$4::numeric,$5,$6::time,$7::time,ARRAY[1,2,3,4,5,6,7,8,9,10,11,12]::smallint[])`,
				append([]any{revision, i + 1}, row...)...); err != nil {
				t.Fatal(err)
			}
		}
	}
	svc := Service{Pool: pool}
	got, err := svc.TOUDay(ctx, owner, touHome, time.Now().UTC())
	if err != nil || got == nil || got.SavingPerKWh.FloatString(2) != "0.20" || got.PeakStart != "16:00" || got.PeakEnd != "21:00" || got.Currency != "USD" {
		t.Fatalf("TOU home: %+v, %v", got, err)
	}
	if got, err = svc.TOUDay(ctx, owner, flatHome, time.Now().UTC()); err != nil || got != nil {
		t.Fatalf("flat home: %+v, %v", got, err)
	}
	if _, err = svc.TOUDay(ctx, uuid.NewString(), touHome, time.Now().UTC()); err != ErrNotFound {
		t.Fatalf("other owner: %v", err)
	}
}
