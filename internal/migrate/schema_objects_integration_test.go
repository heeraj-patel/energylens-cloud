package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func migratedSchemaConn(t *testing.T) (context.Context, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "objs_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	if err = Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return ctx, conn, dsn
}

const oediInsert = `INSERT INTO oedi_utility_zips(edition_year,file_kind,source_row,zip,eiaid,utility_name,state,service_type,ownership,comm_rate,ind_rate,res_rate,dataset_url,published_on,source_url,source_sha256,retrieved_at)
VALUES($1,$2,$3,$4,'1234','Example Power','IL','Bundled',$5,0.1,NULL,0.12,'https://data.openei.org/submissions/8563','2025-11-10T00:00:00Z','https://data.openei.org/files/8563/iou_zipcodes_2024.csv',$6,now())`

func TestOEDIUtilityZipsConstraints(t *testing.T) {
	ctx, conn, _ := migratedSchemaConn(t)
	sha := strings.Repeat("a", 64)
	if _, err := conn.Exec(ctx, oediInsert, 2024, "iou", 1, "60601", "Investor Owned", sha); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	if _, err := conn.Exec(ctx, oediInsert, 2024, "non_iou", 1, "60601", "Municipal", sha); err != nil {
		t.Fatalf("valid non-IOU row rejected: %v", err)
	}
	bad := []struct {
		name string
		args []any
	}{
		{"short zip", []any{2024, "iou", 2, "6060", "Investor Owned", sha}},
		{"bad edition", []any{1999, "iou", 2, "60601", "Investor Owned", sha}},
		{"bad kind", []any{2024, "other", 2, "60601", "Investor Owned", sha}},
		{"zero row", []any{2024, "iou", 0, "60601", "Investor Owned", sha}},
		{"iou not investor owned", []any{2024, "iou", 2, "60601", "Municipal", sha}},
		{"non-iou investor owned", []any{2024, "non_iou", 2, "60601", "Investor Owned", sha}},
		{"bad sha", []any{2024, "iou", 2, "60601", "Investor Owned", "XYZ"}},
		{"duplicate pk", []any{2024, "iou", 1, "60601", "Investor Owned", sha}},
	}
	for _, c := range bad {
		if _, err := conn.Exec(ctx, oediInsert, c.args...); err == nil {
			t.Errorf("%s: invalid row accepted", c.name)
		}
	}
	if _, err := conn.Exec(ctx, `UPDATE oedi_utility_zips SET res_rate=-1 WHERE source_row=1 AND file_kind='iou'`); err == nil {
		t.Error("negative rate accepted")
	}
}

func TestProgramRefreshJobInsertNotifiesAfterCommit(t *testing.T) {
	ctx, conn, dsn := migratedSchemaConn(t)
	listen, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Close(ctx)
	if _, err = listen.Exec(ctx, `LISTEN program_discovery_refresh`); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err = tx.QueryRow(ctx, `INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category) VALUES('US','60601','refrigerator') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	early, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	if n, werr := listen.WaitForNotification(early); werr == nil {
		t.Fatalf("notify delivered before commit: %v", n.Payload)
	}
	cancel()
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		n, werr := listen.WaitForNotification(wait)
		if werr != nil {
			t.Fatalf("no notify after commit: %v", werr)
		}
		if n.Channel == "program_discovery_refresh" && n.Payload == id {
			return
		}
	}
}
