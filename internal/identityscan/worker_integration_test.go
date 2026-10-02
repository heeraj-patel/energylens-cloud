package identityscan

import (
	"context"
	"encoding/json"
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

func TestPGImmediateAndNightlyClaimBoundary(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "identityscan_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dryer','dryer','{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel) VALUES($1,$2,$3,'dryer','Acme','A1','electricity')`, home, appliance, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	store := Store{Pool: pool}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	got, err := store.Claim(ctx, false, now)
	if err != nil || got == nil || got.Attempts != 1 {
		t.Fatalf("fresh claim: %+v %v", got, err)
	}
	if again, err := store.Claim(ctx, false, now.Add(time.Minute)); err != nil || again != nil {
		t.Fatalf("premature re-claim: %+v %v", again, err)
	}
	report := json.RawMessage(`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence"}]}`)
	if err = store.Finish(ctx, *got, report, nil, now); err != nil {
		t.Fatal(err)
	}
	if again, err := store.Claim(ctx, false, now.Add(25*time.Hour)); err != nil || again != nil {
		t.Fatalf("immediate pass retried missing data: %+v %v", again, err)
	}
	if again, err := store.Claim(ctx, true, now.Add(23*time.Hour)); err != nil || again != nil {
		t.Fatalf("early nightly retry: %+v %v", again, err)
	}
	retry, err := store.Claim(ctx, true, now.Add(25*time.Hour))
	if err != nil || retry == nil || retry.Attempts != 2 {
		t.Fatalf("due nightly retry: %+v %v", retry, err)
	}
	if err = store.Finish(ctx, *retry, json.RawMessage(`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"identity_review_candidate"}]}`), nil, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, err := store.Claim(ctx, true, now.Add(49*time.Hour)); err != nil || again != nil {
		t.Fatalf("review-pending retried: %+v %v", again, err)
	}
	for i := 0; i < 2; i++ {
		otherHome, otherAppliance := uuid.NewString(), uuid.NewString()
		if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Another','UTC')`, otherHome, owner); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dryer','dryer','{}')`, otherAppliance, otherHome); err != nil {
			t.Fatal(err)
		}
		key := make([]byte, 32)
		key[0] = byte(i + 1)
		if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel) VALUES($1,$2,$3,'dryer','Acme','A1','electricity')`, otherHome, otherAppliance, key); err != nil {
			t.Fatal(err)
		}
	}
	scanner := &fakeScanner{report: json.RawMessage(`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"identity_review_candidate"}]}`)}
	count, err := store.Run(ctx, scanner, false, 10, now.Add(50*time.Hour))
	if err != nil || count != 2 || scanner.calls != 1 {
		t.Fatalf("cross-home same-model batch dedup: count=%d calls=%d err=%v", count, scanner.calls, err)
	}
	failedKey := make([]byte, 32)
	failedKey[0] = 3
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel) VALUES($1,$2,$3,'dryer','Acme','A2','electricity')`, home, appliance, failedKey); err != nil {
		t.Fatal(err)
	}
	failureNow := now.Add(51 * time.Hour)
	count, err = store.Run(ctx, &fakeScanner{}, false, 10, failureNow)
	if err != nil || count != 1 {
		t.Fatalf("failed source pass: %d %v", count, err)
	}
	var state string
	if err = pool.QueryRow(ctx, `SELECT status FROM identity_scan_requests WHERE home_id=$1 AND model='A2'`, home).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("scanner error mislabeled: %q %v", state, err)
	}
	if claim, err := store.Claim(ctx, true, failureNow.Add(5*time.Minute)); err != nil || claim != nil {
		t.Fatalf("failed command entered missing-source nightly path: %+v %v", claim, err)
	}
	if claim, err := store.Claim(ctx, false, failureNow.Add(16*time.Minute)); err != nil || claim == nil || claim.Model != "A2" {
		t.Fatalf("due transient retry: %+v %v", claim, err)
	}
}
