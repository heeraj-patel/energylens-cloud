package meteredexport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/meteredhistory"
	"energylens.local/cloud/internal/meteredsummary"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type exportPage struct {
	Dataset                 string            `json:"dataset"`
	Retention               string            `json:"retention"`
	ContinuationConsistency string            `json:"continuation_consistency"`
	AsOf                    time.Time         `json:"as_of"`
	Records                 []json.RawMessage `json:"records"`
	NextCursor              *string           `json:"next_cursor"`
}

func TestOwnerMeteredExportKeepsKindsAndScope(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "export_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	owner, other, home, hub, sensor, appliance, attachment := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, item := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_export_test',$2,'test',$3,2)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'plug','plug')`, []any{sensor, home, hub}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Device','refrigerator','{}')`, []any{appliance, home}},
		{`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, []any{appliance, home}},
		{`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,now()-interval '1 day')`, []any{attachment, home, sensor, appliance}},
	} {
		if _, err = pool.Exec(ctx, item.sql, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	end := start.Add(time.Minute)
	batch := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,2,$2,$3,$4)`, hub, batch, home, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for i, kind := range []string{"power_point", "energy_interval", "coverage_gap"} {
		if _, err = pool.Exec(ctx, `INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,2,$2,1,$3,$4,$5,$6,$7)`, hub, ids[i], home, sensor, kind, make([]byte, 32), batch); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,observed_at,watts) VALUES($1,2,$2,1,'power_point',$3,90)`, []any{hub, ids[0], start}},
		{`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel,quality,source) VALUES($1,2,$2,1,'energy_interval',$3,$4,2.3,60,$5,'electricity','metered','verified_driver')`, []any{hub, ids[1], start, end, attachment}},
		{`INSERT INTO coverage_gaps(hub_id,ownership_epoch,event_id,revision,first_at,last_at,reason,dropped_count) VALUES($1,2,$2,1,$3,$4,'byte_limit',2)`, []any{hub, ids[2], start, end}},
	} {
		if _, err = pool.Exec(ctx, item.sql, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	bucket := start.Truncate(time.Hour)
	day := bucket.Truncate(24 * time.Hour)
	if _, err = pool.Exec(ctx, `INSERT INTO energy_daily(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,2,$3,$4,$5,2.3,60,1,'complete','exact','[]')`, home, hub, sensor, attachment, day); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO energy_hourly(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,2,$3,$4,$5,2.3,60,1,'complete','exact','[]')`, []any{home, hub, sensor, attachment, bucket}},
		{`INSERT INTO power_hourly(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,sample_count,min_watts,max_watts,mean_watts,source_refs) VALUES($1,$2,2,$3,$4,1,90,90,90,'[]')`, []any{home, hub, sensor, bucket}},
		{`INSERT INTO coverage_hourly(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,gap_overlap_count,dropped_count_started,source_refs) VALUES($1,$2,2,$3,$4,1,2,'[]')`, []any{home, hub, sensor, bucket}},
	} {
		if _, err = pool.Exec(ctx, item.sql, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte("metered-export-test-key-32-bytes!")
	svc := Service{History: meteredhistory.Store{Pool: pool}, Summary: meteredsummary.Store{Pool: pool}, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner }, CursorKey: key}
	router := chi.NewRouter()
	svc.Routes(router)
	request := func(path string) (int, exportPage) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var p exportPage
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, p
	}
	base := "/homes/" + home + "/metered-export?dataset="
	var historyCursor, summaryCursor string
	for _, dataset := range []string{"history", "hour"} {
		code, page := request(base + dataset + "&limit=1")
		if code != 200 || page.NextCursor == nil || len(page.Records) != 1 || page.AsOf.IsZero() {
			t.Fatalf("%s first page %d %+v", dataset, code, page)
		}
		if dataset == "history" && (page.Retention != "detail_30_days" || page.ContinuationConsistency != "ingest_snapshot") {
			t.Fatalf("history metadata %+v", page)
		}
		if dataset == "hour" && (page.Retention != "summary_24_months" || page.ContinuationConsistency != "live_rollup") {
			t.Fatalf("summary metadata %+v", page)
		}
		if dataset == "history" {
			historyCursor = *page.NextCursor
		} else {
			summaryCursor = *page.NextCursor
		}
		seen := map[string]bool{}
		for {
			for _, raw := range page.Records {
				var item map[string]any
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				kind := item["kind"].(string)
				if seen[kind] {
					t.Fatalf("duplicate %s %s", dataset, kind)
				}
				seen[kind] = true
				if kind == "power_point" && (item["watts"] == nil || item["energy_wh"] != nil) || kind == "energy_interval" && (item["energy_wh"] == nil || item["watts"] != nil) || kind == "coverage_gap" && (item["watts"] != nil || item["energy_wh"] != nil) || kind == "power_sample_stats" && (item["mean_watts"] == nil || item["energy_wh"] != nil) || kind == "energy" && (item["energy_wh"] == nil || item["mean_watts"] != nil) {
					t.Fatalf("mixed units %s: %s", kind, raw)
				}
			}
			if page.NextCursor == nil {
				break
			}
			code, page = request("/homes/" + home + "/metered-export?cursor=" + url.QueryEscape(*page.NextCursor) + "&limit=1")
			if code != 200 {
				t.Fatalf("cursor status %d", code)
			}
		}
		if len(seen) != 3 {
			t.Fatalf("%s kinds: %v", dataset, seen)
		}
	}
	continuation := func(raw string) string {
		return "/homes/" + home + "/metered-export?cursor=" + url.QueryEscape(raw) + "&limit=1"
	}
	for _, original := range []string{historyCursor, summaryCursor} {
		parts := strings.Split(original, ".")
		if len(parts) != 3 || parts[0] != "v1" {
			t.Fatalf("unexpected signed cursor %q", original)
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]any{"SnapshotCursor": float64(999999), "AsOf": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "Sensor": uuid.NewString(), "From": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)} {
			mutated := make(map[string]any, len(fields))
			for k, v := range fields {
				mutated[k] = v
			}
			mutated[name] = value
			b, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if code, _ := request(continuation("v1." + base64.RawURLEncoding.EncodeToString(b) + "." + parts[2])); code != 400 {
				t.Fatalf("%s tampered cursor accepted: %d", name, code)
			}
		}
		afterName := "HistoryAfter"
		if fields["Dataset"] == "hour" {
			afterName = "SummaryAfter"
		}
		fields[afterName].(map[string]any)["Epoch"] = float64(999)
		b, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := request(continuation("v1." + base64.RawURLEncoding.EncodeToString(b) + "." + parts[2])); code != 400 {
			t.Fatalf("%s tampered cursor accepted: %d", afterName, code)
		}
		if code, _ := request(continuation(parts[1])); code != 400 {
			t.Fatalf("legacy unsigned cursor accepted: %d", code)
		}
		if code, _ := request(continuation(original) + "&sensor_id=" + sensor); code != 400 {
			t.Fatalf("cursor filter override accepted: %d", code)
		}
	}
	// A new service instance with the same durable key can continue the page.
	restarted := svc
	restarted.CursorKey = append([]byte(nil), key...)
	router = chi.NewRouter()
	restarted.Routes(router)
	if code, page := request(continuation(historyCursor)); code != 200 || len(page.Records) != 1 || page.AsOf.IsZero() {
		t.Fatalf("restart did not preserve history continuation: %d %+v", code, page)
	}
	if code, page := request(continuation(summaryCursor)); code != 200 || len(page.Records) != 1 {
		t.Fatalf("restart did not preserve summary continuation: %d %+v", code, page)
	}
	rotated := restarted
	rotated.CursorKey, rotated.PreviousCursorKey = []byte("new-metered-export-key-32-bytes!!"), key
	router = chi.NewRouter()
	rotated.Routes(router)
	if code, _ := request(continuation(historyCursor)); code != 200 {
		t.Fatalf("previous key rejected during rotation: %d", code)
	}
	rotated.PreviousCursorKey = nil
	router = chi.NewRouter()
	rotated.Routes(router)
	if code, _ := request(continuation(historyCursor)); code != 400 {
		t.Fatalf("retired key cursor accepted: %d", code)
	}
	router = chi.NewRouter()
	svc.Routes(router)
	code, attached := request(base + "history&attachment_id=" + attachment)
	if code != 200 || len(attached.Records) != 1 {
		t.Fatalf("attached history %d %+v", code, attached)
	}
	code, attached = request(base + "hour&attachment_id=" + attachment)
	if code != 200 || len(attached.Records) != 1 {
		t.Fatalf("attached summary %d %+v", code, attached)
	}
	code, daily := request(base + "day")
	if code != 200 || len(daily.Records) != 1 || daily.Dataset != "day" {
		t.Fatalf("daily export %d %+v", code, daily)
	}
	var dailyRow map[string]any
	if err := json.Unmarshal(daily.Records[0], &dailyRow); err != nil || dailyRow["kind"] != "energy" || dailyRow["energy_wh"] == nil {
		t.Fatalf("daily row %v %v", dailyRow, err)
	}
	for _, path := range []string{base + "history&limit=101", base + "history&from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z", base + "hour&from=2020-01-01T00:00:00Z&to=2020-02-01T00:00:00Z"} {
		if code, _ = request(path); code != 400 {
			t.Fatalf("expected bounded range/limit rejection: %s %d", path, code)
		}
	}
	svc.UserID = func(*http.Request) string { return other }
	router = chi.NewRouter()
	svc.Routes(router)
	if code, _ = request(base + "history"); code != 404 {
		t.Fatalf("foreign history %d", code)
	}
	if code, _ = request(base + "hour"); code != 404 {
		t.Fatalf("foreign summary %d", code)
	}
	if code, _ = request(continuation(historyCursor)); code != 404 {
		t.Fatalf("foreign owner history cursor %d", code)
	}
	if code, _ = request(continuation(summaryCursor)); code != 404 {
		t.Fatalf("foreign owner summary cursor %d", code)
	}
}
