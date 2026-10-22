package programs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type parityProbe struct{}

func (parityProbe) Check(context.Context, RegisteredSource) error {
	return errors.New("unexpected Check call")
}

func (parityProbe) Fetch(_ context.Context, s RegisteredSource) (FetchedSource, error) {
	if strings.Contains(s.Key, "fail") {
		return FetchedSource{}, errors.New("unavailable")
	}
	return FetchedSource{Content: []byte("parity official page"), ContentType: "text/html"}, nil
}

// Datawiz runs the same seed and dump (tests/test_program_parity.py) against parity_expected.json.
// ENERGYLENS_PARITY_UPDATE=1 rewrites the file from this runner.
func TestRefreshWorkerParityRows(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_parity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	seed, err := os.ReadFile("testdata/parity_seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(seed)); err != nil {
		t.Fatal(err)
	}
	worker := RefreshWorker{DB: pool, Resolver: oediFixtureResolverWithDB(t, pool), Probe: parityProbe{}}
	for {
		claimed, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !claimed {
			break
		}
	}
	dump, err := os.ReadFile("testdata/parity_dump.sql")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err = pool.QueryRow(ctx, string(dump)).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ENERGYLENS_PARITY_UPDATE") == "1" {
		if err = os.WriteFile("testdata/parity_expected.json", []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/parity_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
		t.Fatalf("rows differ from testdata/parity_expected.json:\n%s", got)
	}
}

func oediFixtureResolverWithDB(t *testing.T, pool *pgxpool.Pool) OEDIZIPResolver {
	t.Helper()
	r := oediFixtureResolver(t)
	r.DB = pool
	return r
}
