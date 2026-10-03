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

func TestRecommendationEventsOwnerScopeAndRetry(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recommendation_lifecycle_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, device := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Test','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Computer','computer','{"fuel":"electricity","match_quality":"unresolved"}')`, []any{device, home}},
	} {
		if _, err = pool.Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := PGStore{Pool: pool}
	key := uuid.NewString()
	e := LifecycleEvent{ApplianceID: device, RuleID: "computer.sleep_settings", RuleVersion: 1, State: "accepted"}
	if _, err = store.Replay(ctx, other, home, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign replay: %v", err)
	}
	if _, err = store.Record(ctx, other, home, key, e); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign write: %v", err)
	}
	if _, err = store.Record(ctx, owner, home, key, e); err != nil {
		t.Fatal(err)
	}
	if got, readErr := store.Replay(ctx, owner, home, key); readErr != nil || got == nil || got.State != "accepted" {
		t.Fatalf("replay: %+v %v", got, readErr)
	}
	if _, err = store.Record(ctx, owner, home, key, e); err != nil {
		t.Fatalf("same key retry: %v", err)
	}
	changed := e
	changed.State = "dismissed"
	if _, err = store.Record(ctx, owner, home, key, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed key: %v", err)
	}
	if _, err = store.Record(ctx, owner, home, uuid.NewString(), LifecycleEvent{ApplianceID: device, RuleID: e.RuleID, RuleVersion: 1, State: "completed"}); err != nil {
		t.Fatalf("accepted completion: %v", err)
	}
	if _, err = store.Record(ctx, owner, home, uuid.NewString(), e); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal completion: %v", err)
	}
	got, err := store.Latest(ctx, owner, home)
	if err != nil || got[lifecycleKey(device, e.RuleID)].State != "completed" {
		t.Fatalf("latest: %+v %v", got, err)
	}
	if _, err = store.Latest(ctx, other, home); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign list: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET archived_at=$2 WHERE id=$1`, device, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err = store.Latest(ctx, owner, home)
	if err != nil || len(got) != 0 {
		t.Fatalf("archived event leaked: %+v %v", got, err)
	}
}
