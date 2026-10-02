package meteredhistory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerHistorySnapshotPaginationAndTransfer(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "history_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "history-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "history-other"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, other.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	createHome := func(user string) string {
		h, e := accounts.Create(ctx, user, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
		if e != nil {
			t.Fatal(e)
		}
		return h.ID
	}
	home, otherHome := createHome(owner.ID), createHome(other.ID)
	var hub, sensor, appliance, attachment string
	err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_history_test',$1,'test',$2,2) RETURNING id`, make([]byte, 32), home).Scan(&hub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('history-cert',$1,2,now()+interval '1 hour','test')`, hub)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,2,'meter','plug',ARRAY['power','energy']) RETURNING id`, home, hub).Scan(&sensor)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Device','refrigerator','{}') RETURNING id`, home).Scan(&appliance)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(time.Second)
	end := start.Add(time.Minute)
	err = pool.QueryRow(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,1,$4) RETURNING id`, home, sensor, appliance, start).Scan(&attachment)
	if err != nil {
		t.Fatal(err)
	}
	watts := 120.0
	energy := 2.3
	covered := int64(60)
	fuel, quality, source := "electricity", "metered", "verified_driver"
	reason := "byte_limit"
	dropped := int64(2)
	pointID, gapID, intervalID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	point := telemetry.Event{Kind: "power_point", EventID: pointID, Revision: 1, SensorID: sensor, Timestamp: &start, Watts: &watts}
	gap := telemetry.Event{Kind: "coverage_gap", GapID: gapID, Revision: 1, SensorID: sensor, FirstTimestamp: &start, LastTimestamp: &end, Reason: &reason, DroppedCount: &dropped}
	interval := telemetry.Event{Kind: "energy_interval", EventID: intervalID, Revision: 1, SensorID: sensor, Start: &start, End: &end, EnergyWh: &energy, CoveredSeconds: &covered, AttachmentRevisionID: &attachment, Fuel: &fuel, Quality: &quality, Source: &source}
	ingest := telemetry.Store{Pool: pool}
	hash := sha256.Sum256([]byte("history-batch"))
	batch := telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []telemetry.Event{point, gap, interval}}
	if _, err = ingest.Accept(ctx, hub, "history-cert", 2, batch, hash[:]); err != nil {
		t.Fatal(err)
	}
	store := Store{Pool: pool}
	q := Query{HomeID: home, OwnerID: owner.ID, From: now.Add(-time.Hour), To: now.Add(5 * time.Minute), Limit: 100}
	if err = store.Scope(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err = store.Scope(ctx, Query{HomeID: home, OwnerID: other.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner scope: %v", err)
	}
	cutoff, _, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q.SnapshotCursor = cutoff
	rows, err := store.Page(ctx, q)
	if err != nil || len(rows) != 3 {
		t.Fatalf("home history %d %v", len(rows), err)
	}
	kinds := map[string]bool{}
	for _, row := range rows {
		var v map[string]any
		if err = json.Unmarshal(row.Payload, &v); err != nil {
			t.Fatal(err)
		}
		kinds[v["kind"].(string)] = true
		if v["kind"] == "power_point" && v["energy_wh"] != nil {
			t.Fatal("point became energy")
		}
	}
	if !kinds["power_point"] || !kinds["energy_interval"] || !kinds["coverage_gap"] {
		t.Fatalf("missing kind: %v", kinds)
	}
	q.AttachmentID = attachment
	rows, err = store.Page(ctx, q)
	if err != nil || len(rows) != 1 {
		t.Fatalf("attachment history %d %v", len(rows), err)
	}
	q.AttachmentID = ""
	q.SensorID = sensor
	rows, err = store.Page(ctx, q)
	if err != nil || len(rows) != 3 {
		t.Fatalf("sensor history %d %v", len(rows), err)
	}
	correctedWatts := 90.0
	point.Watts = &correctedWatts
	point.Revision = 2
	if _, err = ingest.Accept(ctx, hub, "history-cert", 2, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []telemetry.Event{point}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Page(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !payloadWatts(rows, pointID, 120) {
		t.Fatal("late correction changed frozen snapshot")
	}
	q.SnapshotCursor, _, err = store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = store.Page(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !payloadWatts(rows, pointID, 90) {
		t.Fatal("latest correction missing")
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET home_id=$2,ownership_epoch=3 WHERE id=$1`, hub, otherHome); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Page(ctx, q)
	if err != nil || len(rows) != 3 {
		t.Fatalf("old owner history after transfer %d %v", len(rows), err)
	}
	otherQ := q
	otherQ.HomeID = otherHome
	otherQ.OwnerID = other.ID
	otherQ.SensorID = ""
	rows, err = store.Page(ctx, otherQ)
	if err != nil || len(rows) != 0 {
		t.Fatalf("new owner sees prior epoch %d %v", len(rows), err)
	}
	if err = store.Scope(ctx, Query{HomeID: otherHome, OwnerID: other.ID, SensorID: sensor}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-home sensor: %v", err)
	}
	// HTTP next_cursor carries a stable snapshot and returns bounded pages.
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner.ID }}
	router := chi.NewRouter()
	service.Routes(router)
	u := "/homes/" + home + "/metered-history?from=" + url.QueryEscape(q.From.Format(time.RFC3339)) + "&to=" + url.QueryEscape(q.To.Format(time.RFC3339)) + "&limit=1"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, u, nil))
	if w.Code != 200 {
		t.Fatalf("first page %d %s", w.Code, w.Body.String())
	}
	var firstPage struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor *string           `json:"next_cursor"`
		AsOfCursor int64             `json:"as_of_cursor"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &firstPage); err != nil || len(firstPage.Events) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first page %+v %v", firstPage, err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/metered-history?cursor="+url.QueryEscape(*firstPage.NextCursor)+"&limit=1", nil))
	if w.Code != 200 {
		t.Fatalf("next page %d %s", w.Code, w.Body.String())
	}
}
func payloadWatts(rows []Row, id string, want float64) bool {
	for _, row := range rows {
		if row.EventID != id {
			continue
		}
		var v map[string]any
		if json.Unmarshal(row.Payload, &v) != nil {
			return false
		}
		got, ok := v["watts"].(float64)
		return ok && got == want
	}
	return false
}
