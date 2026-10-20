package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatawizOnePassExportRequiresAbsoluteOutputAndDatabase(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "")
	for _, args := range [][]string{{}, {"-output", "relative/requests"}, {"-output", "/tmp/requests", "extra"}} {
		if err := exportDatawizRequests(context.Background(), args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if err := exportDatawizRequests(context.Background(), []string{"-output", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "ENERGYLENS_CLOUD_DATABASE_URL required") {
		t.Fatalf("database gate: %v", err)
	}
}

func TestQueueDatawizResolveOperator(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "")
	for _, args := range [][]string{{}, {"-appliance", uuid.NewString(), "-output", "relative/path"}} {
		if err := queueDatawizResolve(ctx, args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("invalid args %v: %v", args, err)
		}
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	schema := "queue_resolve_operator_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", parsed.String())
	if err = queueDatawizResolve(ctx, []string{"-appliance", uuid.NewString()}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown appliance: %v", err)
	}
	output := t.TempDir()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = stdout }()
	err = queueDatawizResolve(ctx, []string{"-appliance", appliance, "-output", output})
	write.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Queued  string `json:"queued"`
		Written int    `json:"requests_written"`
	}
	if err = json.NewDecoder(read).Decode(&result); err != nil {
		t.Fatal(err)
	}
	read.Close()
	if result.Queued != appliance || result.Written != 1 {
		t.Fatalf("operator result: %+v", result)
	}
	files, err := filepath.Glob(filepath.Join(output, "resolve", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("resolve files: %v %v", files, err)
	}
	var pending int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_queue`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("queue drain: %d %v", pending, err)
	}
}

func TestRelinkCatalogRequiresNoArgsAndDatabase(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "")
	if err := relinkCatalog(context.Background(), []string{"extra"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("args: %v", err)
	}
	if err := relinkCatalog(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "ENERGYLENS_CLOUD_DATABASE_URL required") {
		t.Fatalf("database gate: %v", err)
	}
}

func TestRequeueDatawizSchemaRequiresNoArgsAndDatabase(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "")
	if err := requeueDatawizSchema(context.Background(), []string{"extra"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("args: %v", err)
	}
	if err := requeueDatawizSchema(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "ENERGYLENS_CLOUD_DATABASE_URL required") {
		t.Fatalf("database gate: %v", err)
	}
}
