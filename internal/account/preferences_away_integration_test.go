package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHomePreferencesAwayScheduleStore(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "away_pref_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	s, prefs := PGStore{Pool: pool}, PGPreferenceStore{Pool: pool}
	owner, err := s.Resolve(ctx, Identity{Issuer: "https://issuer", Subject: "away", Email: "away@example.test", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Resolve(ctx, Identity{Issuer: "https://issuer", Subject: "other"})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("create"))
	home, err := s.Create(ctx, owner.ID, HomeCreate{Name: "House", Timezone: "America/Chicago", Location: Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	initial, err := prefs.GetHomePreferences(ctx, owner.ID, home.ID)
	if err != nil || initial.AwaySchedule != nil || initial.SchemaVersion != 1 {
		t.Fatalf("initial preferences = %+v %v", initial, err)
	}
	blocks := []AwayBlock{{Days: []string{"mon", "tue"}, StartLocal: "08:30", EndLocal: "17:00"}}
	key, h := uuid.NewString(), sha256.Sum256([]byte("patch away"))
	set, err := prefs.PatchHomePreferences(ctx, owner.ID, home.ID, HomePreferencesPatch{AwaySchedule: AwaySchedulePatch{Set: true, Blocks: blocks}}, 1, key, h[:])
	if err != nil || set.Version != 2 || set.SchemaVersion != 2 || !reflect.DeepEqual(set.AwaySchedule, blocks) {
		t.Fatalf("away patch = %+v %v", set, err)
	}
	if got, err := prefs.GetHomePreferences(ctx, owner.ID, home.ID); err != nil || !reflect.DeepEqual(got, set) {
		t.Fatalf("away read = %+v %v", got, err)
	}
	if replay, err := prefs.PatchHomePreferences(ctx, owner.ID, home.ID, HomePreferencesPatch{AwaySchedule: AwaySchedulePatch{Set: true, Blocks: blocks}}, 1, key, h[:]); err != nil || !reflect.DeepEqual(replay, set) {
		t.Fatalf("away replay = %+v %v", replay, err)
	}
	if _, err = prefs.PatchHomePreferences(ctx, owner.ID, home.ID, HomePreferencesPatch{AwaySchedule: AwaySchedulePatch{Set: true}}, 1, uuid.NewString(), h[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale away patch = %v", err)
	}
	goal := "reduce_cost"
	kept, err := prefs.PatchHomePreferences(ctx, owner.ID, home.ID, HomePreferencesPatch{Goal: &goal}, 2, uuid.NewString(), h[:])
	if err != nil || !reflect.DeepEqual(kept.AwaySchedule, blocks) || kept.SchemaVersion != 2 {
		t.Fatalf("goal patch changed the schedule: %+v %v", kept, err)
	}
	if _, err = prefs.PatchHomePreferences(ctx, other.ID, home.ID, HomePreferencesPatch{AwaySchedule: AwaySchedulePatch{Set: true}}, 3, uuid.NewString(), h[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner away patch = %v", err)
	}
	cleared, err := prefs.PatchHomePreferences(ctx, owner.ID, home.ID, HomePreferencesPatch{AwaySchedule: AwaySchedulePatch{Set: true}}, 3, uuid.NewString(), h[:])
	if err != nil || cleared.AwaySchedule != nil || cleared.SchemaVersion != 2 {
		t.Fatalf("clear = %+v %v", cleared, err)
	}
	var revisions, withAway int
	if err = pool.QueryRow(ctx, `SELECT count(*),count(away_schedule) FROM home_preference_revisions WHERE home_id=$1`, home.ID).Scan(&revisions, &withAway); err != nil || revisions != 4 || withAway != 2 {
		t.Fatalf("revisions = %d with away %d %v", revisions, withAway, err)
	}

	if err = s.DeleteHome(ctx, owner.ID, home.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = purgeHomeTx(ctx, tx, home.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	for _, q := range []string{
		`SELECT count(*) FROM home_preferences WHERE home_id=$1`,
		`SELECT count(*) FROM home_preference_revisions WHERE home_id=$1`,
	} {
		if err = tx.QueryRow(ctx, q, home.ID).Scan(&left); err != nil || left != 0 {
			t.Fatalf("%s left %d rows %v", q, left, err)
		}
	}
}
