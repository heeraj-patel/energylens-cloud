package outcomes

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

func TestOwnerScopedActionReplayAndMeasuredEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "outcomes_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	conn, e := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	if e = migrate.Apply(ctx, conn, migrations.FS); e != nil {
		t.Fatal(e)
	}
	ids := make([]string, 7)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	owner, other, home, hub, sensor, appliance, attachment := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	at := time.Now().UTC().Truncate(time.Hour).Add(-8 * 24 * time.Hour)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_outcomes_test',$2,'test',$3,2)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'plug','plug')`, []any{sensor, home, hub}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Device','computer','{}')`, []any{appliance, home}},
		{`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, []any{appliance, home}},
		{`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5)`, []any{attachment, home, sensor, appliance, at.Add(-8 * 24 * time.Hour)}},
	}
	for _, stmt := range statements {
		if _, e = pool.Exec(ctx, stmt.sql, stmt.args...); e != nil {
			t.Fatal(e)
		}
	}
	s := Store{Pool: pool}
	key := uuid.NewString()
	in := Create{ApplianceID: appliance, AttachmentID: attachment, RuleID: "computer.sleep_settings", OccurredAt: at, ComparableUse: true, ComparableWeather: true, Confounders: []string{}}
	if _, e = s.CreateAction(ctx, other, home, key, in); !errors.Is(e, ErrNotFound) {
		t.Fatalf("other owner create: %v", e)
	}
	outsideAttachment := in
	outsideAttachment.OccurredAt = at.Add(-9 * 24 * time.Hour)
	if _, e = s.CreateAction(ctx, owner, home, uuid.NewString(), outsideAttachment); !errors.Is(e, ErrNotFound) {
		t.Fatalf("action before attachment: %v", e)
	}
	a, e := s.CreateAction(ctx, owner, home, key, in)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.CreateAction(ctx, owner, home, key, in)
	if e != nil || replay.ID != a.ID {
		t.Fatalf("replay: %+v %v", replay, e)
	}
	changed := in
	changed.RuleID = "television.quick_start"
	if _, e = s.CreateAction(ctx, owner, home, key, changed); !errors.Is(e, ErrConflict) {
		t.Fatalf("key conflict: %v", e)
	}
	if _, e = s.GetAction(ctx, other, home, a.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("other owner read: %v", e)
	}
	stored, e := s.GetAction(ctx, owner, home, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if stored.SensorID != sensor || stored.HubID != hub || stored.Epoch != 2 {
		t.Fatalf("scope: %+v", stored)
	}
	for i := -hours; i < hours; i++ {
		wh := 10.0
		if i >= 0 {
			wh = 8
		}
		_, e = pool.Exec(ctx, `INSERT INTO energy_hourly(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,2,$3,$4,$5,$6,3600,1,'complete','exact','[]')`, home, hub, sensor, attachment, at.Add(time.Duration(i)*time.Hour), wh)
		if e != nil {
			t.Fatal(e)
		}
	}
	rows, e := s.Buckets(ctx, owner, home, stored)
	if e != nil {
		t.Fatal(e)
	}
	o := Evaluate(stored, rows, time.Now().UTC())
	if o.Status != "observed-change" || o.DeltaWh == nil {
		t.Fatalf("outcome: %+v", o)
	}
	if _, e = pool.Exec(ctx, `UPDATE homes SET owner_user_id=$1 WHERE id=$2`, other, home); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetAction(ctx, owner, home, a.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("old owner after transfer: %v", e)
	}
}
