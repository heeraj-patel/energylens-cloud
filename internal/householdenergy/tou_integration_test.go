package householdenergy

import (
	"context"
	"math/big"
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

func TestTOURevisionPeriodsPriceFromTheDatabase(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "tou_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home := uuid.NewString(), uuid.NewString()
	var revision string
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','America/Los_Angeles')`, []any{home, owner}},
	} {
		if _, err = pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = pool.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
 VALUES($1,1,'electricity',0.30,'kWh','USD','user_tariff','bill',now()-interval '1 day',false,'2026-01-01T00:00:00Z') RETURNING id::text`, home).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	for i, row := range [][]any{
		{"summer peak", "0.15", everyDay, "16:00", "21:00", []int16{6, 7, 8, 9}},
		{"friday late", "0.10", []string{"fri"}, "21:00", "07:00", []int16{10}},
	} {
		if _, err = pool.Exec(ctx, `INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,$2,$3,$4::numeric,$5,$6::time,$7::time,$8::smallint[])`,
			append([]any{revision, i + 1}, row...)...); err != nil {
			t.Fatal(err)
		}
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	fuel := "electricity"
	read := func(from, to time.Time) string {
		t.Helper()
		item := interval{Start: from, End: to, Wh: new(big.Rat).SetFrac64(to.Sub(from).Milliseconds(), 3600), Fuel: &fuel}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, _, schedule, err := readEffectiveTariff(ctx, tx, home, time.Now().UTC(), []interval{item})
		if err != nil || schedule == nil {
			t.Fatalf("schedule %v: %v", schedule, err)
		}
		return priceIntervalRange(item, from, to, *schedule).FloatString(6)
	}
	june := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	if got := read(june, june.Add(2*time.Hour)); got != "0.750000" {
		t.Fatalf("15:00-17:00 = %s", got)
	}
	friday := time.Date(2026, 10, 16, 21, 0, 0, 0, loc)
	if got := read(friday, friday.Add(12*time.Hour)); got != "3.900000" {
		t.Fatalf("Friday 21:00 to Saturday 09:00 = %s", got)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,3,'overlap',0.01,ARRAY['mon'],'20:00','22:00',ARRAY[6]::smallint[])`, revision); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	item := interval{Start: june, End: june.Add(time.Hour), Wh: big.NewRat(1000, 1), Fuel: &fuel}
	if _, _, _, err = readEffectiveTariff(ctx, tx, home, time.Now().UTC(), []interval{item}); err == nil {
		t.Fatal("overlapping period rows were read")
	}
}
