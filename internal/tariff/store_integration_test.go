package tariff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Operator imports use a different home to show official-rate selection
	// without changing the existing customer's user-tariff precedence.
	otherHome, appliance, run := uuid.New(), uuid.New(), uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Other home','UTC')`, otherHome, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'fridge','refrigerator','{}')`, appliance, otherHome); err != nil {
		t.Fatal(err)
	}
	snapshot := []byte("official electricity tariff snapshot version one")
	digest := sha256.Sum256(snapshot)
	verifiedAt := time.Now().UTC().Truncate(time.Second)
	effective := verifiedAt.Add(-35 * 24 * time.Hour).Format(time.RFC3339)
	operatorIn := OfficialImport{OwnerUserID: owner.ID, HomeID: otherHome.String(), SourceURL: "https://utility.example.test/rates/electricity", SourceSHA256: hex.EncodeToString(digest[:]), Tariff: Create{Fuel: "electricity", Rate: "0.18", RateUnit: "kWh", Currency: "USD", RateKind: "official_tariff", SourceAsOf: verifiedAt.Format(time.RFC3339), IsFallback: &fallback, EffectiveFrom: effective}}
	if _, err = ImportOfficial(ctx, pool, OfficialImport{OwnerUserID: other.ID, HomeID: otherHome.String(), SourceURL: operatorIn.SourceURL, SourceSHA256: operatorIn.SourceSHA256, Tariff: operatorIn.Tariff}, snapshot, "operator", verifiedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner official import: %v", err)
	}
	badHash := operatorIn
	badHash.SourceSHA256 = strings.Repeat("0", 64)
	if _, err = ImportOfficial(ctx, pool, badHash, snapshot, "operator", verifiedAt); err == nil {
		t.Fatal("mismatched source snapshot accepted")
	}
	firstOfficial, err := ImportOfficial(ctx, pool, operatorIn, snapshot, "operator", verifiedAt)
	if err != nil || firstOfficial.Revision != 1 || !strings.Contains(firstOfficial.SourceRef, operatorIn.SourceSHA256) {
		t.Fatalf("first official import: %+v %v", firstOfficial, err)
	}
	replayed, err := ImportOfficial(ctx, pool, operatorIn, snapshot, "operator", verifiedAt)
	if err != nil || replayed.ID != firstOfficial.ID {
		t.Fatalf("official replay: %+v %v", replayed, err)
	}
	replayed, err = ImportOfficial(ctx, pool, operatorIn, snapshot, "second-operator", verifiedAt)
	if err != nil || replayed.ID != firstOfficial.ID {
		t.Fatalf("official replay under second operator: %+v %v", replayed, err)
	}
	changedSameSource := operatorIn
	changedSameSource.Tariff.Rate = "0.21"
	if _, err = ImportOfficial(ctx, pool, changedSameSource, snapshot, "operator", verifiedAt); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("same source changed terms: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json)
		VALUES($1,$2,$3,1,'2026-10-01','2026-11-01','UTC','reviewed-energy/v1',$4,'{}','{}')`, run, otherHome, appliance, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	newSnapshot := []byte("official electricity tariff snapshot version two")
	newDigest := sha256.Sum256(newSnapshot)
	changed := operatorIn
	changed.SourceSHA256 = hex.EncodeToString(newDigest[:])
	changed.Tariff.Rate = "0.21"
	secondOfficial, err := ImportOfficial(ctx, pool, changed, newSnapshot, "operator", verifiedAt)
	if err != nil || secondOfficial.Revision != 2 {
		t.Fatalf("updated official rate: %+v %v", secondOfficial, err)
	}
	var reason string
	if err = pool.QueryRow(ctx, `SELECT reason FROM estimate_run_invalidations WHERE run_id=$1`, run).Scan(&reason); err != nil || reason != "tariff_changed" {
		t.Fatalf("estimate not invalidated by official rate: reason=%q err=%v", reason, err)
	}
	selected, err := store.List(ctx, owner.ID, otherHome.String())
	if err != nil || len(selected) != 1 || selected[0].ID != secondOfficial.ID {
		t.Fatalf("changed official rate not selected: %+v %v", selected, err)
	}
}

func TestTariffListElectricitySourceExpiry(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "tariff_expiry_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "tariff-expiry-owner"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	newHome := func(name string) string {
		home, createErr := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: name, Timezone: "UTC", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
		if createErr != nil {
			t.Fatal(createErr)
		}
		return home.ID
	}
	store := PGStore{Pool: pool}
	freshHome := newHome("Fresh electricity")
	boundaryHome := newHome("Boundary electricity and gas")
	futureHome := newHome("Future electricity")
	effectiveHome := newHome("Effective-to boundary")
	insert := func(home, fuel string, revision int, sourceExpr string) string {
		var id string
		err = pool.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
			VALUES($1,$2,$3,0.15,CASE WHEN $3='electricity' THEN 'kWh' ELSE 'therm' END,'USD','user_tariff','owner source',`+sourceExpr+`,false,now()-interval '1 day') RETURNING id`, home, revision, fuel).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// A source checked just before the 60-day limit remains visible.
	freshID := insert(freshHome, "electricity", 1, "now()-interval '1440 hours'+interval '1 minute'")
	listed, err := store.List(ctx, owner.ID, freshHome)
	if err != nil || len(listed) != 1 || listed[0].ID != freshID {
		t.Fatalf("electricity just before expiry: %+v, %v", listed, err)
	}

	// At the exact boundary the electricity source expires; natural gas keeps
	// its existing 18-month source window.
	exactID := insert(boundaryHome, "electricity", 1, "now()-interval '1440 hours'")
	gasID := insert(boundaryHome, "natural_gas", 1, "now()-interval '61 days'")
	listed, err = store.List(ctx, owner.ID, boundaryHome)
	if err != nil || len(listed) != 1 || listed[0].Fuel != "natural_gas" || listed[0].ID != gasID {
		t.Fatalf("exact-boundary electricity / unchanged gas: %+v, %v", listed, err)
	}
	var retained int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tariff_revisions WHERE id=$1`, exactID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("expired historical electricity revision not retained: count=%d err=%v", retained, err)
	}

	// A quarterly refresh need not run for the effective_to cut-off to take
	// effect. The immutable row remains available as source history.
	var effectiveID string
	if err = pool.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from,effective_to)
		VALUES($1,1,'electricity',0.19,'kWh','USD','official_tariff','isolated source',now()-interval '1 day',false,now()-interval '1 day',now()) RETURNING id`, effectiveHome).Scan(&effectiveID); err != nil {
		t.Fatal(err)
	}
	listed, err = store.List(ctx, owner.ID, effectiveHome)
	if err != nil || len(listed) != 0 {
		t.Fatalf("effective_to boundary still selected: %+v %v", listed, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tariff_revisions WHERE id=$1`, effectiveID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("effective_to history missing: count=%d err=%v", retained, err)
	}

	// A future-dated source is never listed, even though it is within the
	// nominal 60-day interval.
	futureID := insert(futureHome, "electricity", 1, "now()+interval '1 minute'")
	listed, err = store.List(ctx, owner.ID, futureHome)
	if err != nil || len(listed) != 0 {
		t.Fatalf("future-dated electricity source listed (%s): %+v, %v", futureID, listed, err)
	}

	// Expiry affects the current list only; creating another revision preserves
	// the immutable historical revision and selects the newer current revision.
	oldID := insert(freshHome, "electricity", 2, "now()-interval '2 days'")
	newID := insert(freshHome, "electricity", 3, "now()-interval '1 day'")
	listed, err = store.List(ctx, owner.ID, freshHome)
	if err != nil || len(listed) != 1 || listed[0].ID != newID {
		t.Fatalf("new current electricity revision: %+v, %v", listed, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity'`, freshHome).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("historical electricity revisions not retained (old=%s): count=%d err=%v", oldID, retained, err)
	}
}
