package meteredsummary

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerEpochKindsAndKeyset(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "summary_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_summary_test',$2,'test',$3,2)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'plug','plug')`, []any{sensor, home, hub}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Device','refrigerator','{}')`, []any{appliance, home}},
		{`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, []any{appliance, home}},
		{`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,now()-interval '1 day')`, []any{attachment, home, sensor, appliance}},
	}
	for _, s := range statements {
		if _, e = pool.Exec(ctx, s.sql, s.args...); e != nil {
			t.Fatal(e)
		}
	}
	bucket := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	ref := `[]`
	for i, sql := range []string{
		`INSERT INTO energy_hourly(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,2,$3,$4,$5,2.3,60,1,'complete','exact',$6)`,
		`INSERT INTO power_hourly(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,sample_count,min_watts,max_watts,mean_watts,source_refs) VALUES($1,$2,2,$3,$4,1,90,90,90,$5)`,
		`INSERT INTO coverage_hourly(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,gap_overlap_count,dropped_count_started,source_refs) VALUES($1,$2,2,$3,$4,1,2,$5)`,
	} {
		args := []any{home, hub, sensor, bucket, ref}
		if i == 0 {
			args = []any{home, hub, sensor, attachment, bucket, ref}
		}
		if _, e = pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	// Daily reads use the daily tables, with the same explicit attachment scope.
	dailyBucket := bucket.Truncate(24 * time.Hour)
	if _, e = pool.Exec(ctx, `INSERT INTO energy_daily(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,2,$3,$4,$5,2.3,60,1,'complete','exact',$6)`, home, hub, sensor, attachment, dailyBucket, ref); e != nil {
		t.Fatal(e)
	}
	store := Store{Pool: pool}
	emptyHome := uuid.NewString()
	if _, e = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Sample','UTC')`, emptyHome, owner); e != nil {
		t.Fatal(e)
	}
	sample, e := NewSampleProvider(map[string]bool{owner: true, other: true}, map[string]bool{home: true, emptyHome: true})
	if e != nil {
		t.Fatal(e)
	}
	router := chi.NewRouter()
	Service{Store: store, Sample: sample, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-User") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return r.Header.Get("X-User") }}.Routes(router)
	samplePath := "/homes/" + emptyHome + "/metered-summaries?granularity=day&from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z"
	sampleRequest := func(user, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if user != "" {
			req.Header.Set("X-User", user)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	wSample := sampleRequest(owner, samplePath)
	if wSample.Code != 200 || wSample.Header().Get("X-EnergyLens-Data-Mode") != "sample" || wSample.Header().Get("X-EnergyLens-Fixture-Revision") != "synthetic_household_v1" || wSample.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sample response: %d %s", wSample.Code, wSample.Body.String())
	}
	var sampleBody struct {
		Granularity string           `json:"granularity"`
		Rows        []map[string]any `json:"summaries"`
		NextCursor  any              `json:"next_cursor"`
	}
	if e = json.Unmarshal(wSample.Body.Bytes(), &sampleBody); e != nil || sampleBody.Granularity != "day" || len(sampleBody.Rows) != 1 || sampleBody.NextCursor != nil || sampleBody.Rows[0]["status"] != "no_intervals" || sampleBody.Rows[0]["energy_wh"] != nil {
		t.Fatalf("sample body: %+v, %v", sampleBody, e)
	}
	if w := sampleRequest("", samplePath); w.Code != 401 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("sample masked authentication: %d", w.Code)
	}
	if w := sampleRequest(other, samplePath); w.Code != 404 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("sample crossed owner: %d", w.Code)
	}
	if w := sampleRequest(owner, samplePath+"&sensor_id=30000000-0000-4000-8000-000000000001"); w.Code != 404 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("sample bypassed sensor scope: %d", w.Code)
	}
	if w := sampleRequest(owner, strings.Replace(samplePath, "granularity=day", "granularity=hour", 1)); w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" || !strings.Contains(w.Body.String(), `"summaries":[]`) {
		t.Fatalf("hour sample leaked day row: %d %s", w.Code, w.Body.String())
	}
	if w := sampleRequest(owner, strings.Replace(samplePath, emptyHome, home, 1)); w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"summaries":[]`) {
		t.Fatalf("real history lost precedence outside query window: %d %s", w.Code, w.Body.String())
	}
	request := func(ownerID, path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-User", ownerID)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w.Code, body
	}
	firstPath := "/homes/" + home + "/metered-summaries?granularity=hour&from=" + url.QueryEscape(bucket.Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(bucket.Add(time.Hour).Format(time.RFC3339)) + "&limit=1"
	status, first := request(owner, firstPath)
	if status != 200 {
		t.Fatalf("first page: %d %v", status, first)
	}
	cursorRaw, ok := first["next_cursor"].(string)
	if !ok {
		t.Fatalf("missing cursor: %v", first)
	}
	continuation := "/homes/" + home + "/metered-summaries?cursor=" + url.QueryEscape(cursorRaw) + "&limit=1"
	status, second := request(owner, continuation)
	if status != 200 || len(second["summaries"].([]any)) != 1 {
		t.Fatalf("unchanged continuation: %d %v", status, second)
	}
	if status, body := request(other, continuation); status != 400 || body["code"] != "invalid_request" {
		t.Fatalf("other owner cursor: %d %v", status, body)
	}
	if status, body := request(owner, "/homes/"+uuid.NewString()+"/metered-summaries?cursor="+url.QueryEscape(cursorRaw)); status != 400 || body["code"] != "invalid_request" {
		t.Fatalf("wrong home cursor: %d %v", status, body)
	}
	if status, body := request(owner, continuation+"&sensor_id="+sensor); status != 400 || body["code"] != "invalid_request" {
		t.Fatalf("wrong query cursor: %d %v", status, body)
	}
	if status, body := request(owner, "/homes/"+home+"/metered-summaries?cursor=bad!"); status != 400 || body["code"] != "invalid_request" {
		t.Fatalf("malformed cursor: %d %v", status, body)
	}
	if _, e = pool.Exec(ctx, `UPDATE energy_hourly SET energy_wh=3.3 WHERE home_id=$1`, home); e != nil {
		t.Fatal(e)
	}
	if status, body := request(owner, continuation); status != 409 || body["code"] != "evidence_changed" {
		t.Fatalf("correction cursor: %d %v", status, body)
	}
	status, first = request(owner, firstPath)
	if status != 200 {
		t.Fatalf("new first page: %d %v", status, first)
	}
	cursorRaw = first["next_cursor"].(string)
	continuation = "/homes/" + home + "/metered-summaries?cursor=" + url.QueryEscape(cursorRaw)
	if _, e = pool.Exec(ctx, `INSERT INTO coverage_hourly(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,gap_overlap_count,dropped_count_started,source_refs) VALUES($1,$2,2,$3,$4,1,1,'[]')`, home, hub, sensor, bucket.Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	if status, body := request(owner, continuation); status != 409 || body["code"] != "evidence_changed" {
		t.Fatalf("late insert cursor: %d %v", status, body)
	}
	if _, e = pool.Exec(ctx, `DELETE FROM coverage_hourly WHERE home_id=$1 AND bucket_start=$2`, home, bucket.Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	q := Query{HomeID: home, OwnerID: owner, Granularity: "hour", From: bucket.Add(-time.Hour), To: bucket.Add(time.Hour), Limit: 1}
	if e = store.Scope(ctx, q); e != nil {
		t.Fatal(e)
	}
	otherQ := q
	otherQ.OwnerID = other
	if e = store.Scope(ctx, otherQ); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross owner: %v", e)
	}
	seen := map[string]bool{}
	for {
		rows, err := store.Page(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		var payload map[string]any
		if err = json.Unmarshal(rows[0].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		kind := payload["kind"].(string)
		if seen[kind] {
			t.Fatal("duplicate cursor row")
		}
		seen[kind] = true
		if kind != "energy" && payload["energy_wh"] != nil {
			t.Fatal("non energy row contains energy")
		}
		q.After = &rows[0].Key
	}
	dailyQ := Query{HomeID: home, OwnerID: owner, Granularity: "day", From: dailyBucket, To: dailyBucket.Add(24 * time.Hour), Limit: 5}
	dailyRows, dailyErr := store.Page(ctx, dailyQ)
	if dailyErr != nil || len(dailyRows) != 1 || dailyRows[0].Key.Kind != "energy" {
		t.Fatalf("daily summary %v %v", dailyRows, dailyErr)
	}
	if len(seen) != 3 {
		t.Fatalf("missing kind: %v", seen)
	}
	q.After = nil
	q.AttachmentID = attachment
	rows, e := store.Page(ctx, q)
	if e != nil || len(rows) != 1 || rows[0].Key.Kind != "energy" {
		t.Fatalf("attachment scope %v %v", rows, e)
	}
	// Transfer keeps the old home and epoch visible only to its owner.
	otherHome := uuid.NewString()
	if _, e = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Other','UTC')`, otherHome, other); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE hubs SET home_id=$1,ownership_epoch=3 WHERE id=$2`, otherHome, hub); e != nil {
		t.Fatal(e)
	}
	q.AttachmentID = ""
	rows, e = store.Page(ctx, q)
	if e != nil || len(rows) != 2 {
		t.Fatalf("old epoch %v %v", rows, e)
	}
	otherQ.HomeID = otherHome
	rows, e = store.Page(ctx, otherQ)
	if e != nil || len(rows) != 0 {
		t.Fatalf("new owner inherited old epoch %v %v", rows, e)
	}
	pool.Close()
	if w := sampleRequest(owner, samplePath); w.Code != 500 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("sample masked database failure: %d %s", w.Code, w.Body.String())
	}
}
