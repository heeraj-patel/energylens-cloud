package tariff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTariffOwnerReplayAndImmutableRevisions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "tariff_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "tariff-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "tariff-other"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	if _, err = store.List(ctx, other.ID, home.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner list: %v", err)
	}
	fallback := false
	fixed := "12.50"
	now := time.Now().UTC().Format(time.RFC3339)
	in := Create{Fuel: "electricity", Rate: "0.159", RateUnit: "kWh", Currency: "USD", RateKind: "user_tariff", SourceRef: "bill", SourceAsOf: now, IsFallback: &fallback, FixedCharge: &fixed, EffectiveFrom: now}
	key := uuid.NewString()
	hash := bytes.Repeat([]byte{1}, 32)
	if _, err = store.Create(ctx, other.ID, home.ID, in, key, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner create: %v", err)
	}
	first, err := store.Create(ctx, owner.ID, home.ID, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.FixedCharge == nil || *first.FixedCharge != "12.50" {
		t.Fatalf("first tariff %+v", first)
	}
	replay, err := store.Create(ctx, owner.ID, home.ID, in, key, hash)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, in, key, bytes.Repeat([]byte{2}, 32)); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key conflict: %v", err)
	}
	second, err := store.Create(ctx, owner.ID, home.ID, in, uuid.NewString(), hash)
	if err != nil || second.Revision != 2 {
		t.Fatalf("second revision %+v %v", second, err)
	}
	list, err := store.List(ctx, owner.ID, home.ID)
	if err != nil || len(list) != 1 || list[0].ID != second.ID || list[0].Rate != "0.15900000" || list[0].FixedCharge == nil || *list[0].FixedCharge != "12.5000" {
		t.Fatalf("list %+v %v", list, err)
	}
	// Trusted sourced rows can coexist, but the customer tariff takes priority.
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,3,'electricity',0.2,'kWh','USD','regional_average','official EIA',now(),true,now()-interval '1 day')`, home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'natural_gas',1.2,'therm','USD','regional_average','old source',now()-interval '2 years',true,now()-interval '2 years')`, home.ID); err != nil {
		t.Fatal(err)
	}
	list, err = store.List(ctx, owner.ID, home.ID)
	if err != nil || len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("current priority and stale source: %+v %v", list, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE tariff_revisions SET rate=0 WHERE id=$1`, first.ID); err == nil {
		t.Fatal("tariff revision changed")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM tariff_revisions WHERE id=$1`, first.ID); err == nil {
		t.Fatal("tariff revision deleted")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,3,'natural_gas',1,'kWh','USD','user_tariff','bill',now(),false,now())`, home.ID); err == nil {
		t.Fatal("mismatched fuel unit stored")
	}
}
