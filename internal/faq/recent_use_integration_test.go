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

func TestCurrentRecentUseOwnerRevisionAndUnknownSupersession(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "faq_recent_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Washer','washer','{}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	qid := "faq-contributors/v1.washer.recent_cycles"
	insert := func(version, revision int, status, value string) {
		t.Helper()
		var raw any
		if status == "confirmed" {
			raw = value
		}
		var confirmed any
		if status == "confirmed" {
			confirmed = time.Now().Add(-time.Hour)
		}
		if _, e := pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,$3,$4,1,$5,$6,$7::jsonb,$8)`, home, appliance, version, qid, revision, status, raw, confirmed); e != nil {
			t.Fatal(e)
		}
	}
	store := PGStore{Pool: pool}
	insert(1, 1, "confirmed", "4")
	if got, err := store.CurrentRecentUse(ctx, other, home, nil); err != nil || len(got) != 0 {
		t.Fatalf("cross owner: %+v %v", got, err)
	}
	got, err := store.CurrentRecentUse(ctx, owner, home, nil)
	if err != nil || len(got) != 1 || got[0].Status != "confirmed" {
		t.Fatalf("current: %+v %v", got, err)
	}
	insert(1, 2, "unknown", "")
	got, err = store.CurrentRecentUse(ctx, owner, home, nil)
	if err != nil || len(got) != 1 || got[0].Status != "unknown" {
		t.Fatalf("latest unknown: %+v %v", got, err)
	}
	if f, _ := RecentUseFindings(got, time.Now()); len(f) != 0 {
		t.Fatalf("unknown finding: %+v", f)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2 WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	got, err = store.CurrentRecentUse(ctx, owner, home, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("old identity: %+v %v", got, err)
	}
	insert(2, 3, "confirmed", "0")
	got, err = store.CurrentRecentUse(ctx, owner, home, &appliance)
	if err != nil || len(got) != 1 || got[0].ApplianceVersion != 2 {
		t.Fatalf("new identity: %+v %v", got, err)
	}
	if _, err = store.CurrentEstimate(ctx, other, home, nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner gate: %v", err)
	}
}
