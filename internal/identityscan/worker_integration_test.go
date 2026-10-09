package identityscan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	identity := `{"brand":"Acme","full_model":"A1","fuel":"electricity","match_quality":"confirmed_exact"}`
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dryer','dryer',$3::jsonb)`, appliance, home, identity); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel) VALUES($1,$2,$3,'dryer','Acme','A1','electricity')`, home, appliance, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	secondAppliance := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Second dryer','dryer',$3::jsonb)`, secondAppliance, home, identity); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_subscriptions(appliance_id,home_id,request_id,identity_key)
SELECT $1,q.home_id,q.id,q.identity_key FROM identity_scan_requests q WHERE q.home_id=$2 AND q.model='A1'`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_subscriptions(appliance_id,home_id,request_id,identity_key)
SELECT $1,q.home_id,q.id,q.identity_key FROM identity_scan_requests q WHERE q.home_id=$2 AND q.model='A1'`, secondAppliance, home); err != nil {
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
	if again, err := store.Claim(ctx, false, now.Add(23*time.Hour)); err != nil || again != nil {
		t.Fatalf("retried missing data before due time: %+v %v", again, err)
	}
	retry, err := store.Claim(ctx, false, now.Add(25*time.Hour))
	if err != nil || retry == nil || retry.Attempts != 2 {
		t.Fatalf("missed nightly retry was not recovered by a later pass: %+v %v", retry, err)
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
	transientRetry, err := store.Claim(ctx, false, failureNow.Add(16*time.Minute))
	if err != nil || transientRetry == nil || transientRetry.Model != "A2" {
		t.Fatalf("due transient retry: %+v %v", transientRetry, err)
	}
	if err = store.Finish(ctx, *transientRetry, json.RawMessage(`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A2","status":"identity_review_candidate"}]}`), nil, failureNow.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Only a completed KB no-evidence cycle enters the human research file.
	finalReport := `{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence"}]}`
	if _, err = pool.Exec(ctx, `UPDATE identity_scan_requests SET status='missing_source',attempt_count=5,next_attempt_at=NULL,candidate_report=$2,updated_at=$3 WHERE home_id=$1 AND model='A1'`, home, finalReport, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE identity_scan_requests SET status='missing_source',attempt_count=4,next_attempt_at=$2,candidate_report=$3 WHERE home_id<>$1 AND model='A1'`, home, now.Add(24*time.Hour), finalReport); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unresolved_device_help.jsonl")
	exported, err := ExportHelp(ctx, pool, path)
	if err != nil || exported != 2 {
		t.Fatalf("final KB export: %d %v", exported, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), `"appliance_id":"`+appliance+`"`) || !strings.Contains(string(content), `"appliance_id":"`+secondAppliance+`"`) || strings.Contains(string(content), `"home_id"`) || strings.Contains(string(content), `"model":"A2"`) {
		t.Fatalf("export content: %q %v", content, err)
	}

	for i := 0; i < 12; i++ {
		jobAppliance := uuid.NewString()
		if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,$3,'dryer','{}')`, jobAppliance, home, fmt.Sprintf("Dryer %d", i)); err != nil {
			t.Fatal(err)
		}
		key := make([]byte, 32)
		key[0], key[1] = 12, byte(i)
		if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel) VALUES($1,$2,$3,'dryer','Acme',$4,'electricity')`, home, jobAppliance, key, fmt.Sprintf("Drain%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	drainScanner := modelScanner{}
	firstPass, err := store.Run(ctx, drainScanner, false, 10, now.Add(60*time.Hour))
	if err != nil || firstPass != 10 {
		t.Fatalf("bounded recovery first pass: %d %v", firstPass, err)
	}
	secondPass, err := store.Run(ctx, drainScanner, false, 10, now.Add(61*time.Hour))
	if err != nil || secondPass != 4 {
		t.Fatalf("bounded recovery catch-up pass (two additional due retries plus backlog): %d %v", secondPass, err)
	}

	crashID, crashAppliance := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Crashed dryer','dryer','{}')`, crashAppliance, home); err != nil {
		t.Fatal(err)
	}
	crashKey := make([]byte, 32)
	crashKey[0] = 99
	if _, err = pool.Exec(ctx, `INSERT INTO identity_scan_requests(id,home_id,appliance_id,identity_key,category,manufacturer,model,fuel,status,attempt_count,claim_count,claimed_at) VALUES($1,$2,$3,$4,'dryer','Acme','Crash5','electricity','running',4,5,$5)`, crashID, home, crashAppliance, crashKey, now.Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx, false, now); err != nil {
		t.Fatalf("expired fifth claim recovery: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT status FROM identity_scan_requests WHERE id=$1`, crashID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("fifth crash was not a transient failure: %q %v", state, err)
	}
	var crashAttempts int
	if err = pool.QueryRow(ctx, `SELECT attempt_count FROM identity_scan_requests WHERE id=$1`, crashID).Scan(&crashAttempts); err != nil || crashAttempts != 4 {
		t.Fatalf("unfinished fifth call was counted as completed/final: %d %v", crashAttempts, err)
	}
}

type modelScanner struct{}

func (modelScanner) Scan(_ context.Context, r Request) (json.RawMessage, error) {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":1,"model_count":1,"models":[{"category":%q,"manufacturer":%q,"model":%q,"status":"identity_review_candidate"}]}`, r.Category, r.Manufacturer, r.Model)), nil
}
