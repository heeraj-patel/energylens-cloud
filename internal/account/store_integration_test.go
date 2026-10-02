package account

import (
	"context"
	"crypto/sha256"
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

// Uses an isolated schema in a real Postgres when the integration DSN is set.
// It exercises both a fresh install and the 0016/0017 upgrade path.
func TestAccountHomesFreshAndUpgrade(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			ctx := context.Background()
			schema := "account_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
			if upgrade {
				for _, name := range []string{"0016_energy_rate_cache.sql", "0017_energy_rating_catalog.sql"} {
					body, e := migrations.FS.ReadFile(name)
					if e != nil {
						t.Fatal(e)
					}
					if _, e = pool.Exec(ctx, string(body)); e != nil {
						t.Fatal(e)
					}
				}
				if _, err = pool.Exec(ctx, `INSERT INTO energy_rate_cache(region,rate_usd_per_kwh,period) VALUES('TX',0.15,'2026-07-01')`); err != nil {
					t.Fatal(err)
				}
				if _, err = pool.Exec(ctx, `INSERT INTO energy_rating_catalog(brand,model,category,source_kind,annual_kwh,source_ref,contract_version) VALUES('A','B','fridge','epa_exact',100,'source','v1')`); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				var rates, ratings int
				if err = pool.QueryRow(ctx, "SELECT count(*) FROM energy_rate_cache").Scan(&rates); err != nil {
					t.Fatal(err)
				}
				if err = pool.QueryRow(ctx, "SELECT count(*) FROM energy_rating_catalog").Scan(&ratings); err != nil {
					t.Fatal(err)
				}
				if rates != 1 || ratings != 1 {
					t.Fatalf("existing rows changed: rates=%d ratings=%d", rates, ratings)
				}
			}
			s := PGStore{Pool: pool}
			a, err := s.Resolve(ctx, Identity{Issuer: "https://issuer-a", Subject: "same", Email: "first@example.test", EmailVerified: true})
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.Resolve(ctx, Identity{Issuer: "https://issuer-b", Subject: "same", Email: "first@example.test", EmailVerified: true})
			if err != nil {
				t.Fatal(err)
			}
			if a.ID == b.ID {
				t.Fatal("issuer was omitted from identity key")
			}
			changed, err := s.Resolve(ctx, Identity{Issuer: "https://issuer-a", Subject: "same", Email: "second@example.test", EmailVerified: true})
			if err != nil {
				t.Fatal(err)
			}
			if changed.ID != a.ID || changed.Email == nil || *changed.Email != "second@example.test" {
				t.Fatal("mutable email changed stable identity")
			}
			in := HomeCreate{Name: "House", Timezone: "America/Chicago", Location: Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}
			key := uuid.NewString()
			hash := sha256.Sum256([]byte("create"))
			prefs := PGPreferenceStore{Pool: pool}
			accountPref, err := prefs.GetAccountPreferences(ctx, a.ID)
			if err != nil || accountPref.Version != 1 || accountPref.HistoryDisclosureAcceptedAt != nil {
				t.Fatalf("initial account preferences = %+v %v", accountPref, err)
			}
			if _, err = s.Create(ctx, a.ID, in, key, hash[:]); !errors.Is(err, ErrDisclosureRequired) {
				t.Fatalf("create before disclosure = %v", err)
			}
			enabled := true
			accepted := time.Now().UTC().Format(time.RFC3339)
			prefHash := sha256.Sum256([]byte("pref"))
			updatedAccount, err := prefs.PatchAccountPreferences(ctx, a.ID, AccountPreferencesPatch{EmailReportsEnabled: &enabled, HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), prefHash[:])
			if err != nil || updatedAccount.Version != 2 || !updatedAccount.EmailReportsEnabled || updatedAccount.HistoryDisclosureAcceptedAt == nil {
				t.Fatalf("account preference patch = %+v %v", updatedAccount, err)
			}
			home, err := s.Create(ctx, a.ID, in, key, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			replay, err := s.Create(ctx, a.ID, in, key, hash[:])
			if err != nil || replay.ID != home.ID {
				t.Fatalf("create replay: %v %v", replay.ID, err)
			}
			if _, err = pool.Exec(ctx, `UPDATE account_preferences SET history_disclosure_accepted_at=NULL WHERE user_id=$1`, a.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Create(ctx, a.ID, in, key, hash[:]); !errors.Is(err, ErrDisclosureRequired) {
				t.Fatalf("create replay without disclosure = %v", err)
			}
			nameBefore := "Denied rename"
			if _, err = s.Patch(ctx, b.ID, home.ID, HomePatch{Name: &nameBefore}, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-owner before disclosure = %v", err)
			}
			if _, err = s.Patch(ctx, a.ID, home.ID, HomePatch{Name: &nameBefore}, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrDisclosureRequired) {
				t.Fatalf("patch without disclosure = %v", err)
			}
			if _, err = pool.Exec(ctx, `UPDATE account_preferences SET history_disclosure_accepted_at=now() WHERE user_id=$1`, a.ID); err != nil {
				t.Fatal(err)
			}
			otherHash := sha256.Sum256([]byte("other"))
			if _, err = s.Create(ctx, a.ID, in, key, otherHash[:]); !errors.Is(err, ErrKeyReuse) {
				t.Fatalf("key reuse = %v", err)
			}
			if _, err = s.Get(ctx, b.ID, home.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-owner get = %v", err)
			}
			if got, err := s.List(ctx, b.ID); err != nil || len(got) != 0 {
				t.Fatalf("cross-owner list = %v %v", got, err)
			}
			name := "New name"
			if _, err = s.Patch(ctx, b.ID, home.ID, HomePatch{Name: &name}, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-owner patch = %v", err)
			}
			updated, err := s.Patch(ctx, a.ID, home.ID, HomePatch{Name: &name}, 1, uuid.NewString(), hash[:])
			if err != nil || updated.Version != 2 || updated.Name != name {
				t.Fatalf("owner patch = %+v %v", updated, err)
			}
			if _, err = s.Patch(ctx, a.ID, home.ID, HomePatch{Name: &name}, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrVersion) {
				t.Fatalf("stale patch = %v", err)
			}
			accountPref, err = prefs.GetAccountPreferences(ctx, a.ID)
			if err != nil || accountPref.Version != 2 || accountPref.HistoryDisclosureAcceptedAt == nil {
				t.Fatalf("accepted account preferences = %+v %v", accountPref, err)
			}
			homePref, err := prefs.GetHomePreferences(ctx, a.ID, home.ID)
			if err != nil || homePref.Version != 1 || homePref.Goal != "unspecified" {
				t.Fatalf("initial home preferences = %+v %v", homePref, err)
			}
			if _, err = prefs.GetHomePreferences(ctx, b.ID, home.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-owner preference read = %v", err)
			}
			prefKey := uuid.NewString()
			goal := "reduce_cost"
			changedPref, err := prefs.PatchHomePreferences(ctx, a.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, prefKey, prefHash[:])
			if err != nil || changedPref.Version != 2 || changedPref.Goal != goal {
				t.Fatalf("home preference patch = %+v %v", changedPref, err)
			}
			replayPref, err := prefs.PatchHomePreferences(ctx, a.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, prefKey, prefHash[:])
			if err != nil || replayPref != changedPref {
				t.Fatalf("home preference replay = %+v %v", replayPref, err)
			}
			if _, err = prefs.PatchHomePreferences(ctx, a.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, prefKey, otherHash[:]); !errors.Is(err, ErrKeyReuse) {
				t.Fatalf("home preference key reuse = %v", err)
			}
			if _, err = prefs.PatchHomePreferences(ctx, a.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, uuid.NewString(), prefHash[:]); !errors.Is(err, ErrVersion) {
				t.Fatalf("stale home preference patch = %v", err)
			}
			if _, err = prefs.PatchHomePreferences(ctx, b.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, prefKey, prefHash[:]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-owner preference patch = %v", err)
			}
			if _, err = prefs.PatchAccountPreferences(ctx, a.ID, AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 2, uuid.NewString(), otherHash[:]); !errors.Is(err, ErrDisclosureAccepted) {
				t.Fatalf("repeated disclosure = %v", err)
			}
			if _, err = pool.Exec(ctx, `UPDATE homes SET deleted_at=now() WHERE id=$1`, home.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = prefs.PatchHomePreferences(ctx, a.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 1, prefKey, prefHash[:]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted home replay = %v", err)
			}
		})
	}
}
