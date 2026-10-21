package householdenergy

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/energyrate"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEffectiveGasRate(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "gas_rate_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "gas-owner"})
	if err != nil {
		t.Fatal(err)
	}
	newHome := func(country, postal string) string {
		h, e := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: country, PostalCode: postal, Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
		if e != nil {
			t.Fatal(e)
		}
		return h.ID
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	insert := func(home, fuel, unit, kind string, rate float64, rev int, fallback bool, from time.Time, to *time.Time) {
		_, e := pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from,effective_to) VALUES($1,$2,$3,$4,$5,'USD',$6,'bill',$7,$8,$9,$10)`,
			home, rev, fuel, rate, unit, kind, now.Add(-time.Hour), fallback, from, to)
		if e != nil {
			t.Fatal(e)
		}
	}
	var calls []string
	resolver := func(basis string, usd float64, err error) GasRateResolver {
		return func(_ context.Context, zip string) (energyrate.GasResolved, error) {
			calls = append(calls, zip)
			return energyrate.GasResolved{USDPerTherm: usd, Basis: basis}, err
		}
	}
	read := func(home string, resolve GasRateResolver) *GasRate {
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		got, e := readEffectiveGasRate(ctx, tx, home, now, resolve)
		if e != nil {
			t.Fatal(e)
		}
		return got
	}
	want := func(t *testing.T, got *GasRate, rate, basis string) {
		t.Helper()
		if got == nil || got.Basis != basis || got.USDPerTherm.Cmp(mustDecimal(rate)) != 0 {
			t.Fatalf("gas rate = %+v, want %s %s", got, rate, basis)
		}
	}

	t.Run("user tariff wins over the resolver", func(t *testing.T) {
		home := newHome("US", "78732")
		insert(home, "natural_gas", "therm", "user_tariff", 1.1, 1, false, now.Add(-48*time.Hour), nil)
		insert(home, "natural_gas", "therm", "user_tariff", 1.3, 2, false, now.Add(-24*time.Hour), nil)
		insert(home, "natural_gas", "therm", "user_tariff", 9, 3, false, now.Add(24*time.Hour), nil)
		calls = nil
		want(t, read(home, resolver(energyrate.GasBasisState, 2, nil)), "1.3", GasRateBasisUserTariff)
		if len(calls) != 0 {
			t.Fatalf("resolver called despite a user tariff: %v", calls)
		}
	})
	t.Run("ended and fallback rows are ignored", func(t *testing.T) {
		home := newHome("US", "78732")
		ended := now.Add(-time.Hour)
		insert(home, "natural_gas", "therm", "user_tariff", 1.1, 1, false, now.Add(-48*time.Hour), &ended)
		insert(home, "natural_gas", "therm", "regional_average", 1.2, 2, true, now.Add(-48*time.Hour), nil)
		want(t, read(home, resolver(energyrate.GasBasisState, 1.55, nil)), "1.55", energyrate.GasBasisState)
	})
	t.Run("state average", func(t *testing.T) {
		calls = nil
		want(t, read(newHome("US", "78732-1234"), resolver(energyrate.GasBasisState, 1.5, nil)), "1.5", energyrate.GasBasisState)
		if len(calls) != 1 || calls[0] != "78732" {
			t.Fatalf("resolver zip = %v", calls)
		}
	})
	t.Run("national average", func(t *testing.T) {
		want(t, read(newHome("US", "78732"), resolver(energyrate.GasBasisNational, 1.4, nil)), "1.4", energyrate.GasBasisNational)
	})
	t.Run("static", func(t *testing.T) {
		want(t, read(newHome("US", "78732"), resolver(energyrate.GasBasisStatic, 1.481, nil)), "1.481", energyrate.GasBasisStatic)
	})
	t.Run("non-US home gets none", func(t *testing.T) {
		calls = nil
		if got := read(newHome("CA", "M5V 2T6"), resolver(energyrate.GasBasisStatic, 1.481, nil)); got != nil || len(calls) != 0 {
			t.Fatalf("non-US got %+v calls=%v", got, calls)
		}
	})
	t.Run("unavailable or invalid resolver result gets none", func(t *testing.T) {
		home := newHome("US", "78732")
		for _, r := range []GasRateResolver{nil, resolver("", 0, energyrate.ErrTemporarilyUnavailable), resolver(energyrate.GasBasisStatic, 0, errors.New("x")), resolver(energyrate.GasBasisStatic, 0, nil)} {
			if got := read(home, r); got != nil {
				t.Fatalf("expected none, got %+v", got)
			}
		}
	})
	t.Run("electricity is unchanged by a gas tariff", func(t *testing.T) {
		home := newHome("US", "78732")
		fuel := "electricity"
		start := now.Add(-2 * time.Hour)
		items := []interval{{Start: start, End: start.Add(time.Hour), Wh: big.NewRat(1000, 1), Fuel: &fuel}}
		insert(home, "natural_gas", "therm", "user_tariff", 1.3, 1, false, now.Add(-24*time.Hour), nil)
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		id, rev, tariff, e := readEffectiveTariff(ctx, tx, home, now, items)
		if e != nil || id != "" || rev != 0 || tariff != nil {
			t.Fatalf("gas row leaked into electricity: id=%q rev=%d tariff=%+v err=%v", id, rev, tariff, e)
		}
		insert(home, "electricity", "kWh", "user_tariff", 0.2, 1, false, now.Add(-24*time.Hour), nil)
		_, rev, tariff, e = readEffectiveTariff(ctx, tx, home, now, items)
		if e != nil || rev != 1 || tariff == nil || len(tariff.Segments) != 1 || tariff.Segments[0].Rate.Cmp(big.NewRat(1, 5)) != 0 {
			t.Fatalf("electricity tariff = rev %d %+v err=%v", rev, tariff, e)
		}
	})
}
