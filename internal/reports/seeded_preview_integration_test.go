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

	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSeededReportPreviewInventoryRouteAndDependencies(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "seeded_report_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	owner, other, home, otherHome, persisted := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		if _, err = pool.Exec(ctx, `INSERT INTO users(id,email) VALUES($1,'private@example.com')`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]uuid.UUID{{home, owner}, {otherHome, other}} {
		if _, err = pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Private','America/Chicago')`, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Persisted','washer','{}')`, persisted, home); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location).Format("2006-01-02")
	end := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, location).Format("2006-01-02")
	estimate, _ := json.Marshal(map[string]any{
		"kind": "estimated_useful_cost", "period": map[string]string{"start": start, "end": end},
		"energy":        map[string]string{"value": "4", "unit": "kWh"},
		"cost":          map[string]string{"value": "1.25", "currency": "USD"},
		"quality":       map[string]string{"source": "reviewed_exact"},
		"evidence_refs": []SourceRef{{ID: uuid.NewString(), Kind: "reviewed_usage_fact", Revision: 1}, {ID: uuid.NewString(), Kind: "tariff", Revision: 1}},
	})
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json,as_of)
		VALUES($1,$2,$3,1,$4,$5,'America/Chicago','reviewed-energy/v1',$6,'{}',$7,$8)`, uuid.New(), home, persisted, start, end, bytes.Repeat([]byte{1}, 32), estimate, now); err != nil {
		t.Fatal(err)
	}
	snapshot := householdenergy.MobileDemoCostSnapshot()
	if err = householdenergy.ImportDemoCostSnapshot(ctx, pool, owner.String(), home.String(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err = householdenergy.ImportDemoCostSnapshot(ctx, pool, other.String(), otherHome.String(), snapshot); err != nil {
		t.Fatal(err)
	}

	store := Store{DB: pool}
	auth := func(next http.Handler) http.Handler { return next }
	actor := func(r *http.Request) string {
		if r.Header.Get("Authorization") == "Bearer other" {
			return other.String()
		}
		return owner.String()
	}
	router := chi.NewRouter()
	(inventory.Service{Store: inventory.PGStore{Pool: pool}, Auth: auth, UserID: actor}).Routes(router)
	(Service{Store: store, Auth: auth, UserID: actor}).Routes(router)
	call := func(method, path, bearer string, body []byte, key uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != uuid.Nil {
			req.Header.Set("Idempotency-Key", key.String())
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	list := call("GET", "/homes/"+home.String()+"/appliances/", "owner", nil, uuid.Nil)
	if list.Code != 200 {
		t.Fatalf("inventory: %d %s", list.Code, list.Body.String())
	}
	var inventoryBody struct {
		Appliances []inventory.Appliance `json:"appliances"`
	}
	if err = json.Unmarshal(list.Body.Bytes(), &inventoryBody); err != nil {
		t.Fatal(err)
	}
	seeded := []uuid.UUID{}
	for _, row := range inventoryBody.Appliances {
		if row.DemoDeviceID != "" {
			seeded = append(seeded, uuid.MustParse(row.ID))
		}
	}
	if len(seeded) != 10 {
		t.Fatalf("named inventory count = %d", len(seeded))
	}
	create := func(ids []uuid.UUID, key uuid.UUID, actor string) *httptest.ResponseRecorder {
		rawIDs := make([]string, len(ids))
		for i, id := range ids {
			rawIDs[i] = id.String()
		}
		body, _ := json.Marshal(map[string]any{"appliance_ids": rawIDs})
		return call("POST", "/homes/"+home.String()+"/reports/preview", actor, body, key)
	}
	key := uuid.New()
	created := create([]uuid.UUID{seeded[0]}, key, "owner")
	if created.Code != 201 {
		t.Fatalf("seeded create: %d %s", created.Code, created.Body.String())
	}
	t.Logf("SEEDED_REPORT_JSON:%s", bytes.TrimSpace(created.Body.Bytes()))
	var p Preview
	if err = json.Unmarshal(created.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.SchemaVersion != "manual-preview/v2" || len(p.Items) != 1 || p.Items[0].ApplianceID != seeded[0].String() || p.Items[0].Category != "other" || p.Items[0].Estimate != nil || p.Items[0].Measured != nil || p.MeasuredStatus != "unavailable" || p.RecommendationsStatus != "unavailable" || p.Stale {
		t.Fatalf("seeded payload: %+v", p)
	}
	for _, forbidden := range []string{"demo-device-01", "useful_cost_micros", "waste_cost_micros", "Study Desktop", "private@example.com"} {
		if strings.Contains(created.Body.String(), forbidden) {
			t.Fatalf("private or synthetic source data leaked: %q", forbidden)
		}
	}
	if digest, err := snapshotDigest(p); err != nil || digest != p.SnapshotSHA256 {
		t.Fatalf("digest: %s %v", digest, err)
	}
	var demoID *string
	var dependencyDigest []byte
	var applianceVersion *int64
	if err = pool.QueryRow(ctx, `SELECT demo_device_id,demo_snapshot_sha256,appliance_version FROM report_preview_items WHERE preview_id=$1`, p.ID).Scan(&demoID, &dependencyDigest, &applianceVersion); err != nil || demoID == nil || *demoID != "demo-device-01" || len(dependencyDigest) != 32 || applianceVersion != nil {
		t.Fatalf("dependency: %v %v %d %v", err, demoID, len(dependencyDigest), applianceVersion)
	}
	var persistedCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM appliances WHERE id=$1`, seeded[0]).Scan(&persistedCount); err != nil || persistedCount != 0 {
		t.Fatalf("seeded appliance persisted: %d %v", persistedCount, err)
	}
	read := call("GET", "/homes/"+home.String()+"/reports/previews/"+p.ID, "owner", nil, uuid.Nil)
	var current Preview
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &current) != nil || current.Stale || current.SnapshotSHA256 != p.SnapshotSHA256 {
		t.Fatalf("seeded read: %d %s", read.Code, read.Body.String())
	}
	if replay := create([]uuid.UUID{seeded[0]}, key, "owner"); replay.Code != 201 || !strings.Contains(replay.Body.String(), p.ID) {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	if conflict := create([]uuid.UUID{seeded[1]}, key, "owner"); conflict.Code != 409 {
		t.Fatalf("key conflict: %d", conflict.Code)
	}
	for _, ids := range [][]uuid.UUID{{seeded[0], persisted}, seeded, {persisted}} {
		w := create(ids, uuid.New(), "owner")
		if w.Code != 201 {
			t.Fatalf("selection %d: %d %s", len(ids), w.Code, w.Body.String())
		}
		var selected Preview
		if err = json.Unmarshal(w.Body.Bytes(), &selected); err != nil {
			t.Fatal(err)
		}
		if len(selected.Items) != len(ids) {
			t.Fatalf("selection count %d: %d", len(ids), len(selected.Items))
		}
		for _, item := range selected.Items {
			if item.ApplianceID == persisted.String() && (item.Estimate == nil || item.Estimate.CostUSD != "1.25") {
				t.Fatalf("persisted estimate lost in selection: %+v", item)
			}
			if item.ApplianceID != persisted.String() && (item.Estimate != nil || item.Measured != nil) {
				t.Fatalf("seeded item claimed evidence: %+v", item)
			}
		}
	}
	for _, ids := range [][]uuid.UUID{{uuid.New()}, {seeded[0], seeded[0]}} {
		if w := create(ids, uuid.New(), "owner"); w.Code != 400 {
			t.Fatalf("invalid selection: %d", w.Code)
		}
	}
	otherRows, err := inventory.DemoAppliances(snapshot, otherHome.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{uuid.MustParse(otherRows[0].ID), uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://energylens.local/demo/"+home.String()+"/demo-device-11"))} {
		if w := create([]uuid.UUID{id}, uuid.New(), "owner"); w.Code != 400 {
			t.Fatalf("foreign/unnamed id: %d", w.Code)
		}
	}
	if w := create([]uuid.UUID{seeded[0]}, uuid.New(), "other"); w.Code != 404 {
		t.Fatalf("cross-owner: %d", w.Code)
	}
	for _, raw := range []string{"demo-device-01", strings.ToUpper(seeded[0].String()), uuid.Nil.String()} {
		body, _ := json.Marshal(map[string]any{"appliance_ids": []string{raw}})
		if w := call("POST", "/homes/"+home.String()+"/reports/preview", "owner", body, uuid.New()); w.Code != 400 {
			t.Fatalf("bad id %q: %d", raw, w.Code)
		}
	}

	queue := func(preview Preview, recipient string) error {
		_, err := store.QueueDelivery(ctx, owner.String(), home.String(), DeliveryInput{PreviewID: preview.ID,
			ApprovedSnapshotSHA256: preview.SnapshotSHA256, Recipients: []string{recipient}, Approved: true, IdempotencyKey: uuid.New()}, now)
		return err
	}
	if err = queue(p, "internal1@example.com"); err != nil {
		t.Fatalf("current queue: %v", err)
	}
	provider := &fakeReportProvider{}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now); err != nil || !processed || provider.calls != 1 {
		t.Fatalf("current worker: %v %v calls=%d", processed, err, provider.calls)
	}
	if err = queue(p, "internal2@example.com"); err != nil {
		t.Fatalf("second current queue: %v", err)
	}
	changed := householdenergy.MobileDemoCostSnapshot()
	changed.Devices[0].Default.Useful++
	changedBody, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	// This direct replacement keeps the home version fixed to isolate the
	// synthetic digest check from the separate home-version check.
	if _, err = pool.Exec(ctx, `UPDATE homes SET demo_cost_snapshot=$2::jsonb WHERE id=$1`, home, changedBody); err != nil {
		t.Fatal(err)
	}
	read = call("GET", "/homes/"+home.String()+"/reports/previews/"+p.ID, "owner", nil, uuid.Nil)
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &current) != nil || !current.Stale {
		t.Fatalf("replaced snapshot read: %d %s", read.Code, read.Body.String())
	}
	if err = queue(p, "internal3@example.com"); !errors.Is(err, ErrStalePreview) {
		t.Fatalf("replaced snapshot queue: %v", err)
	}
	if processed, err := (DeliveryWorker{DB: pool, Provider: provider, Enabled: true}).DeliverOne(ctx, now); err != nil || !processed || provider.calls != 1 {
		t.Fatalf("stale worker: %v %v calls=%d", processed, err, provider.calls)
	}
	if err = householdenergy.ImportDemoCostSnapshot(ctx, pool, owner.String(), home.String(), changed); err != nil {
		t.Fatal(err)
	}
	newPreview, err := store.Create(ctx, owner.String(), home.String(), []uuid.UUID{seeded[0]}, uuid.New(), now)
	if err != nil || newPreview.Stale {
		t.Fatalf("new snapshot preview: %+v %v", newPreview, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET version=version+1 WHERE id=$1`, home); err != nil {
		t.Fatal(err)
	}
	versionStale, err := store.Get(ctx, owner.String(), home.String(), newPreview.ID, now)
	if err != nil || !versionStale.Stale {
		t.Fatalf("home version did not stale preview: %+v %v", versionStale, err)
	}
	removalPreview, err := store.Create(ctx, owner.String(), home.String(), []uuid.UUID{seeded[0]}, uuid.New(), now)
	if err != nil || removalPreview.Stale {
		t.Fatalf("pre-removal preview: %+v %v", removalPreview, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET demo_cost_snapshot=NULL WHERE id=$1`, home); err != nil {
		t.Fatal(err)
	}
	read = call("GET", "/homes/"+home.String()+"/reports/previews/"+removalPreview.ID, "owner", nil, uuid.Nil)
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &current) != nil || !current.Stale {
		t.Fatalf("removed snapshot read: %d %s", read.Code, read.Body.String())
	}
}
