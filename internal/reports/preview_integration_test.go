package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type reportVerifier struct{}

type fakeReportProvider struct {
	calls int
	last  RenderedReport
}

type blockingReportProvider struct {
	started chan struct{}
	release chan struct{}
	id      string
}

func (p *blockingReportProvider) SendReport(ctx context.Context, id string, _ []string, _ RenderedReport) error {
	p.id = id
	close(p.started)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *fakeReportProvider) SendReport(_ context.Context, _ string, _ []string, report RenderedReport) error {
	p.calls++
	p.last = report
	return nil
}

func (reportVerifier) Verify(_ context.Context, token string) (account.Identity, error) {
	if token != "owner" && token != "other" {
		return account.Identity{}, errors.New("bad token")
	}
	return account.Identity{Issuer: "test", Subject: token}, nil
}

func TestManualPreviewOwnerIsolationPrivacyAndStaleness(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "reports_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, appliance, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		if _, err = pool.Exec(ctx, `INSERT INTO users(id,email) VALUES($1,'private@example.com')`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []struct {
		user    uuid.UUID
		subject string
	}{{owner, "owner"}, {other, "other"}} {
		if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,'test',$2)`, identity.user, identity.subject); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Secret home label','America/Chicago')`, home, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,city,county,precision,source)
		VALUES($1,1,'US','78732','Austin','Travis','address','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Serial SECRET-123','123 Main St','{"serial":"SECRET-123"}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	// HTTP handlers use the wall clock; keep the store fixture in the same
	// current home month so this test remains valid across calendar days.
	now := time.Now().UTC().Truncate(time.Second)
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	local := now.In(location)
	periodStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location()).Format("2006-01-02")
	periodEnd := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, local.Location()).Format("2006-01-02")
	fact, tariff := uuid.New(), uuid.New()
	result := map[string]any{"kind": "estimated_useful_cost", "period": map[string]string{"start": periodStart, "end": periodEnd}, "energy": map[string]string{"value": "0", "unit": "kWh"}, "cost": map[string]string{"value": "0", "currency": "USD"}, "quality": map[string]string{"source": "reviewed_exact"}, "evidence_refs": []SourceRef{{ID: fact.String(), Kind: "reviewed_usage_fact", Revision: 1}, {ID: tariff.String(), Kind: "tariff", Revision: 1}}, "private_note": "SECRET-123"}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json,as_of)
		VALUES($1,$2,$3,1,$4,$5,'America/Chicago','reviewed-energy/v1',$6,'{}',$7,$8)`, run, home, appliance, periodStart, periodEnd, bytes.Repeat([]byte{1}, 32), body, now); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: pool}
	if _, err = s.Create(ctx, other.String(), home.String(), []uuid.UUID{appliance}, uuid.New(), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner create: %v", err)
	}
	if _, err = s.Create(ctx, owner.String(), home.String(), []uuid.UUID{uuid.New()}, uuid.New(), now); !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("foreign appliance selection: %v", err)
	}
	key := uuid.New()
	p, err := s.Create(ctx, owner.String(), home.String(), []uuid.UUID{appliance}, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].Estimate == nil || p.Items[0].Estimate.CostUSD != "0" || p.HomeTotalStatus != "unavailable_incomplete_scope" ||
		p.RecommendationsStatus != "available" || len(p.Recommendations) != 1 || p.Recommendations[0].RuleID != "general.gather_information" || p.Recommendations[0].ApplianceID != appliance.String() {
		t.Fatalf("preview: %+v", p)
	}
	replayed, err := s.Create(ctx, owner.String(), home.String(), []uuid.UUID{appliance}, key, now.Add(time.Minute))
	if err != nil || replayed.ID != p.ID {
		t.Fatalf("idempotent replay: %+v %v", replayed, err)
	}
	if _, err = s.Create(ctx, owner.String(), home.String(), []uuid.UUID{uuid.New()}, key, now); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse: %v", err)
	}
	delivery := DeliveryInput{PreviewID: p.ID, ApprovedSnapshotSHA256: p.SnapshotSHA256, Recipients: []string{"Recipient@Example.com"}, Approved: true, IdempotencyKey: uuid.New()}
	if _, err = s.QueueDelivery(ctx, other.String(), home.String(), delivery, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner delivery: %v", err)
	}
	badApproval := delivery
	badApproval.Approved = false
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), badApproval, now); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("missing approval: %v", err)
	}
	badDigest := delivery
	badDigest.ApprovedSnapshotSHA256 = strings.Repeat("0", 64)
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), badDigest, now); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	queued, err := s.QueueDelivery(ctx, owner.String(), home.String(), delivery, now)
	if err != nil || queued.State != "pending" || len(queued.Recipients) != 1 || queued.Recipients[0] != "recipient@example.com" {
		t.Fatalf("delivery request: %+v %v", queued, err)
	}
	again, err := s.QueueDelivery(ctx, owner.String(), home.String(), delivery, now.Add(time.Minute))
	if err != nil || again.ID != queued.ID {
		t.Fatalf("idempotent delivery: %+v %v", again, err)
	}
	reuse := delivery
	reuse.Recipients = []string{"different@example.com"}
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), reuse, now); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("delivery key reuse: %v", err)
	}
	duplicate := delivery
	duplicate.IdempotencyKey = uuid.New()
	duplicate.Recipients = []string{"RECIPIENT@example.com"}
	if same, err := s.QueueDelivery(ctx, owner.String(), home.String(), duplicate, now); err != nil || same.ID != queued.ID {
		t.Fatalf("duplicate delivery: %+v %v", same, err)
	}
	for _, address := range []string{"second@example.com", "third@example.com"} {
		otherDelivery := delivery
		otherDelivery.IdempotencyKey = uuid.New()
		otherDelivery.Recipients = []string{address}
		if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), otherDelivery, now); err != nil {
			t.Fatal(err)
		}
	}
	fourth := delivery
	fourth.IdempotencyKey = uuid.New()
	fourth.Recipients = []string{"fourth@example.com"}
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), fourth, now); !errors.Is(err, ErrDeliveryRate) {
		t.Fatalf("rate bound: %v", err)
	}
	{
		accounts := account.Service{Store: account.PGStore{Pool: pool}, Verifier: reportVerifier{}}
		router := chi.NewRouter()
		Service{Store: s, Auth: accounts.Auth, UserID: account.UserID}.Routes(router)
		path := "/homes/" + home.String() + "/reports/previews/" + p.ID + "/deliveries"
		body := `{"approved_snapshot_sha256":"` + p.SnapshotSHA256 + `","recipients":["Recipient@Example.com"],"approved":true}`
		call := func(token, key, path, body string) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			return w
		}
		if w := call("", delivery.IdempotencyKey.String(), path, body); w.Code != 401 {
			t.Fatalf("unauthenticated enqueue: %d", w.Code)
		}
		if w := call("other", delivery.IdempotencyKey.String(), path, body); w.Code != 404 {
			t.Fatalf("cross-owner enqueue: %d %s", w.Code, w.Body.String())
		}
		if w := call("owner", delivery.IdempotencyKey.String(), path, body); w.Code != 201 || !strings.Contains(w.Body.String(), queued.ID) {
			t.Fatalf("idempotent enqueue: %d %s", w.Code, w.Body.String())
		}
		if w := call("owner", delivery.IdempotencyKey.String(), path, strings.Replace(body, "Recipient@Example.com", "changed@example.com", 1)); w.Code != 409 {
			t.Fatalf("key reuse API: %d %s", w.Code, w.Body.String())
		}
		if w := call("owner", uuid.NewString(), path, strings.Replace(body, p.SnapshotSHA256, strings.Repeat("0", 64), 1)); w.Code != 409 {
			t.Fatalf("digest mismatch API: %d %s", w.Code, w.Body.String())
		}
		if w := call("owner", uuid.NewString(), path, strings.Replace(body, `"approved":true`, `"approved":false`, 1)); w.Code != 400 {
			t.Fatalf("approval API: %d %s", w.Code, w.Body.String())
		}
		if w := call("owner", uuid.NewString(), path, strings.Replace(body, "Recipient@Example.com", "fourth@example.com", 1)); w.Code != 429 {
			t.Fatalf("rate API: %d %s", w.Code, w.Body.String())
		}
	}
	provider := &fakeReportProvider{}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider}).DeliverOne(ctx, now); !errors.Is(err, ErrDeliveryDisabled) || processed || provider.calls != 0 {
		t.Fatalf("delivery enabled by default: %v %v", processed, err)
	}
	// Delivery holds the home and preview dependencies through the provider
	// call. A tombstone, appliance edit, or estimate invalidation must wait.
	blocking := &blockingReportProvider{started: make(chan struct{}), release: make(chan struct{})}
	workerResult := make(chan error, 1)
	go func() {
		_, workerErr := (DeliveryWorker{DB: pool, Provider: blocking, Enabled: true}).DeliverOne(ctx, now)
		workerResult <- workerErr
	}()
	select {
	case <-blocking.started:
	case workerErr := <-workerResult:
		close(blocking.release)
		t.Fatalf("delivery failed before provider: %v", workerErr)
	case <-time.After(5 * time.Second):
		close(blocking.release)
		t.Fatal("delivery did not reach fake provider")
	}
	for _, lock := range []struct {
		name  string
		query string
		id    uuid.UUID
	}{
		{"home tombstone", `SELECT id FROM homes WHERE id=$1 FOR UPDATE NOWAIT`, home},
		{"appliance edit", `SELECT id FROM appliances WHERE id=$1 FOR UPDATE NOWAIT`, appliance},
		{"estimate invalidation", `SELECT id FROM estimate_runs WHERE id=$1 FOR KEY SHARE NOWAIT`, run},
	} {
		lockTx, lockErr := pool.Begin(ctx)
		if lockErr != nil {
			close(blocking.release)
			t.Fatal(lockErr)
		}
		var lockedID string
		lockErr = lockTx.QueryRow(ctx, lock.query, lock.id).Scan(&lockedID)
		_ = lockTx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
			close(blocking.release)
			t.Fatalf("%s escaped delivery transaction: %v", lock.name, lockErr)
		}
	}
	for _, mutation := range []struct {
		name  string
		query string
		id    uuid.UUID
	}{
		{"home tombstone", `UPDATE homes SET deleted_at=$2,version=version+1 WHERE id=$1`, home},
		{"estimate invalidation", `INSERT INTO estimate_run_invalidations(run_id,reason) VALUES($1,'dependency_changed')`, run},
	} {
		mutationTx, mutationErr := pool.Begin(ctx)
		if mutationErr != nil {
			close(blocking.release)
			t.Fatal(mutationErr)
		}
		_, mutationErr = mutationTx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
		if mutationErr == nil {
			if mutation.name == "home tombstone" {
				_, mutationErr = mutationTx.Exec(ctx, mutation.query, mutation.id, now)
			} else {
				_, mutationErr = mutationTx.Exec(ctx, mutation.query, mutation.id)
			}
		}
		_ = mutationTx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(mutationErr, &pgErr) || pgErr.Code != "55P03" {
			close(blocking.release)
			t.Fatalf("%s committed during provider call: %v", mutation.name, mutationErr)
		}
	}
	close(blocking.release)
	if workerErr := <-workerResult; workerErr != nil {
		t.Fatalf("blocked delivery: %v", workerErr)
	}
	// Reset the first row to pending to exercise the normal fake provider path.
	if _, err = pool.Exec(ctx, `UPDATE report_delivery_requests SET state='pending',sent_at=NULL WHERE id=$1`, blocking.id); err != nil {
		t.Fatal(err)
	}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now); err != nil || !processed || provider.calls != 1 || provider.last.SnapshotSHA256 != p.SnapshotSHA256 || !strings.Contains(provider.last.HTML, p.ID) {
		t.Fatalf("fake delivery: %v %v %+v", processed, err, provider)
	}
	var sentCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM report_delivery_requests WHERE state='sent'`).Scan(&sentCount); err != nil || sentCount != 1 {
		t.Fatalf("sent count %d %v", sentCount, err)
	}
	// The provider's documented idempotency window is 24 hours. Once the
	// preview's shorter-or-equal lifetime ends, even a pending retry must stop.
	if _, err = pool.Exec(ctx, `UPDATE report_delivery_requests SET state='pending',sent_at=NULL WHERE id=$1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now.Add(25*time.Hour)); err != nil || !processed || provider.calls != 1 {
		t.Fatalf("expired retry reached provider: processed=%v err=%v calls=%d", processed, err, provider.calls)
	}
	stored, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-123", "Secret home label", "private@example.com", "78732", "Austin", "Travis", "123 Main St"} {
		if bytes.Contains(stored, []byte(secret)) {
			t.Fatalf("private value %q entered preview", secret)
		}
		if strings.Contains(provider.last.HTML, secret) {
			t.Fatalf("private value %q entered rendered email", secret)
		}
	}
	if _, err = s.Get(ctx, other.String(), home.String(), p.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner read: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE report_previews SET payload='{}' WHERE id=$1`, p.ID); err == nil {
		t.Fatal("preview mutation accepted")
	}
	if _, err = pool.Exec(ctx, `UPDATE report_preview_items SET appliance_version=2 WHERE preview_id=$1`, p.ID); err == nil {
		t.Fatal("dependency mutation accepted")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_run_invalidations(run_id,reason) VALUES($1,'dependency_changed')`, run); err != nil {
		t.Fatal(err)
	}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now.Add(time.Hour)); err != nil || !processed || provider.calls != 1 {
		t.Fatalf("stale queued delivery reached provider: %v %v calls=%d", processed, err, provider.calls)
	}
	staleDelivery := delivery
	staleDelivery.IdempotencyKey = uuid.New()
	staleDelivery.Recipients = []string{"stale@example.com"}
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), staleDelivery, now.Add(time.Hour)); !errors.Is(err, ErrStalePreview) {
		t.Fatalf("stale delivery: %v", err)
	}
	read, err := s.Get(ctx, owner.String(), home.String(), p.ID, now.Add(time.Hour))
	if err != nil || !read.Stale || read.Items[0].Estimate == nil {
		t.Fatalf("stale immutable read: %+v %v", read, err)
	}
	replayed, err = s.Create(ctx, owner.String(), home.String(), []uuid.UUID{appliance}, key, now.Add(time.Hour))
	if err != nil || !replayed.Stale || replayed.ID != p.ID {
		t.Fatalf("stale replay: %+v %v", replayed, err)
	}
	if _, err = s.Get(ctx, owner.String(), home.String(), p.ID, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	accounts := account.Service{Store: account.PGStore{Pool: pool}, Verifier: reportVerifier{}}
	router := chi.NewRouter()
	Service{Store: s, Auth: accounts.Auth, UserID: account.UserID}.Routes(router)
	api := func(method, path, body, token string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	path := "/homes/" + home.String() + "/reports/preview"
	request := `{"appliance_ids":["` + appliance.String() + `"]}`
	if got := api(http.MethodPost, path, request, ""); got != 401 {
		t.Fatalf("unauthenticated API create = %d", got)
	}
	if got := api(http.MethodPost, path, request, "other"); got != 404 {
		t.Fatalf("cross-owner API create = %d", got)
	}
	if got := api(http.MethodPost, path, request, "owner"); got != 201 {
		t.Fatalf("owner API create = %d", got)
	}
	readPath := "/homes/" + home.String() + "/reports/previews/" + p.ID
	if got := api(http.MethodGet, readPath, "", "other"); got != 404 {
		t.Fatalf("cross-owner API read = %d", got)
	}
	if got := api(http.MethodGet, readPath, "", "owner"); got != 200 {
		t.Fatalf("owner API read = %d", got)
	}
	// A complete UTC daily bucket is included only for a single mapped meter.
	// Its exact rollup provenance is hashed in the immutable report snapshot.
	bucket := time.Date(local.Year(), local.Month(), 2, 0, 0, 0, 0, time.UTC)
	measuredAt := bucket.Add(5 * 24 * time.Hour)
	hub, sensor, attachment := uuid.New(), uuid.New(), uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_report_test',$2,'test',$3,2)`, hub, make([]byte, 32), home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'report-plug','plug')`, sensor, home, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}') ON CONFLICT DO NOTHING`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5)`, attachment, home, sensor, appliance, bucket.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO energy_daily(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs,computed_at) VALUES($1,$2,2,$3,$4,$5,1200,86400,1,'complete','exact','[]',$6)`, home, hub, sensor, attachment, bucket, measuredAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	measuredPreview, err := s.Create(ctx, owner.String(), home.String(), []uuid.UUID{appliance}, uuid.New(), measuredAt)
	if err != nil || len(measuredPreview.Items) != 1 || measuredPreview.Items[0].Measured == nil || measuredPreview.Items[0].Measured.EnergyKWh != "1.2000000000000000" || measuredPreview.MeasuredStatus != "partial" || len(measuredPreview.Items[0].Measured.SourceSHA256) != 64 {
		t.Fatalf("measured v2 preview: %+v %v", measuredPreview, err)
	}
	measuredHTML, err := RenderApprovedHTML(measuredPreview, measuredPreview.SnapshotSHA256)
	if err != nil || !strings.Contains(measuredHTML.HTML, measuredPreview.Items[0].Measured.SourceSHA256) || !strings.Contains(measuredHTML.HTML, "1.2000000000000000 kWh") {
		t.Fatalf("measured provenance missing from report: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE energy_daily SET energy_wh=1300,computed_at=$2 WHERE attachment_id=$1`, attachment, measuredAt); err != nil {
		t.Fatal(err)
	}
	measuredAfterCorrection, err := s.Get(ctx, owner.String(), home.String(), measuredPreview.ID, measuredAt)
	if err != nil || !measuredAfterCorrection.Stale {
		t.Fatalf("corrected meter rollup did not stale report: %+v %v", measuredAfterCorrection, err)
	}
	// A new usage answer can change a recommendation without changing the
	// appliance or estimate run. Pending v2 snapshots must become stale.
	newPreview, err := s.Create(ctx, owner.String(), home.String(), []uuid.UUID{appliance}, uuid.New(), now.Add(time.Minute))
	if err != nil || newPreview.SchemaVersion != "manual-preview/v2" || newPreview.Stale {
		t.Fatalf("new v2 preview: %+v %v", newPreview, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'computer.hours_per_week',1,1,'confirmed','7',$3)`, home, appliance, now); err != nil {
		t.Fatal(err)
	}
	changedPreview, err := s.Get(ctx, owner.String(), home.String(), newPreview.ID, now.Add(time.Minute))
	if err != nil || !changedPreview.Stale {
		t.Fatalf("usage revision did not stale report: %+v %v", changedPreview, err)
	}
	var invalidationReason string
	if err = pool.QueryRow(ctx, `SELECT reason FROM report_preview_invalidations WHERE preview_id=$1`, newPreview.ID).Scan(&invalidationReason); err != nil || invalidationReason != "recommendation_evidence_changed" {
		t.Fatalf("report invalidation: %q %v", invalidationReason, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET deleted_at=$2,version=version+1 WHERE id=$1`, home, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A pending row can survive a tombstone; the worker must never send it.
	if _, err = pool.Exec(ctx, `UPDATE report_delivery_requests SET state='pending',sent_at=NULL WHERE id=$1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	providerCalls := provider.calls
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now.Add(2*time.Hour)); err != nil || processed || provider.calls != providerCalls {
		t.Fatalf("tombstoned pending delivery reached provider: %v %v calls=%d", processed, err, provider.calls)
	}
	if _, err = s.QueueDelivery(ctx, owner.String(), home.String(), delivery, now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstoned home replayed delivery: %v", err)
	}
}
