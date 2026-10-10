package householdenergy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
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

// The companion Hub test writes an actual CloudOutbox/build_payload wire batch.
// This test takes those bytes through the Cloud upload route, PostgreSQL, and
// the owner dashboard. Both environment variables are set by the cross-repo run.
func TestHubProducedBatchDatabaseRoundTrip(t *testing.T) {
	dsn, batchPath := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL"), os.Getenv("ENERGYLENS_HUB_E2E_BATCH_PATH")
	if dsn == "" || batchPath == "" {
		t.Skip("isolated PostgreSQL URL and Hub-produced batch path are required")
	}
	raw, err := os.ReadFile(batchPath)
	if err != nil {
		t.Fatal(err)
	}
	var batch telemetry.Batch
	if err = json.Unmarshal(raw, &batch); err != nil || len(batch.Events) != 3 || batch.OwnershipEpoch != 2 {
		t.Fatalf("Hub batch shape: events=%d epoch=%d err=%v", len(batch.Events), batch.OwnershipEpoch, err)
	}
	var interval *telemetry.Event
	kinds := map[string]int{}
	for i := range batch.Events {
		e := &batch.Events[i]
		kinds[e.Kind]++
		if e.Kind == "energy_interval" {
			interval = e
		}
	}
	if kinds["power_point"] != 1 || kinds["coverage_gap"] != 1 || kinds["energy_interval"] != 1 || interval == nil || interval.Start == nil || interval.End == nil || interval.AttachmentRevisionID == nil || interval.EnergyWh == nil || *interval.EnergyWh != 2.5 {
		t.Fatalf("Hub did not emit the expected point, gap, and verified interval: %+v", kinds)
	}

	const hubID = "da17c425-d948-4d6b-8cc0-acf84da38c39" // Hub fixture identity.
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "hub_cloud_e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "hub-cloud-db-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "Hub E2E", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_test',$2,'test',$3,2)`, hubID, make([]byte, 32), home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('abc',$1,2,now()+interval '1 hour','test')`, hubID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,$3,2,'local-meter','plug',ARRAY['power','energy'])`, interval.SensorID, home.ID, hubID); err != nil {
		t.Fatal(err)
	}
	var appliance string
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Metered test load','refrigerator','{}') RETURNING id`, home.ID).Scan(&appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5)`, *interval.AttachmentRevisionID, home.ID, interval.SensorID, appliance, interval.Start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','test bill',$2,false,$3)`, home.ID, interval.Start.Add(-24*time.Hour), interval.Start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	upload := chi.NewRouter()
	upload.Route("/v1", (telemetry.Service{Store: telemetry.Store{Pool: pool}}).HubRoutes)
	peerURI, _ := url.Parse("spiffe://energylens.local/hubs/" + hubID + "/epochs/2")
	peer := &x509.Certificate{SerialNumber: big.NewInt(0xabc), URIs: []*url.URL{peerURI}}
	post := func() telemetry.Acknowledgement {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/hubs/"+hubID+"/sync/batches", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
		w := httptest.NewRecorder()
		upload.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Hub batch upload: %d %s", w.Code, w.Body.String())
		}
		var ack telemetry.Acknowledgement
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}
	ack := post()
	if len(ack.AcceptedEventIDs) != len(batch.Events) || ack.Cursor < 1 || ack.BatchID != batch.BatchID {
		t.Fatalf("incomplete Cloud acknowledgement: %+v", ack)
	}
	replay := post()
	if replay.Cursor != ack.Cursor || !replay.AcknowledgedAt.Equal(ack.AcknowledgedAt) {
		t.Fatalf("Hub retry changed durable acknowledgement: first=%+v replay=%+v", ack, replay)
	}
	var batches, events, intervals, gaps int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM ingest_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM ingest_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_observations WHERE kind='energy_interval' AND energy_wh=2.5 AND covered_seconds=60`).Scan(&intervals); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM coverage_gaps`).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || events != 3 || intervals != 1 || gaps != 1 {
		t.Fatalf("Hub events not stored exactly once: batches=%d events=%d intervals=%d gaps=%d", batches, events, intervals, gaps)
	}

	ownerAPI := Service{Pool: pool, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner.ID }, Now: func() time.Time { return interval.End.Add(time.Second) }}
	ownerRoutes := chi.NewRouter()
	ownerRoutes.Route("/v1", ownerAPI.Routes)
	req := httptest.NewRequest(http.MethodGet, "/v1/homes/"+home.ID+"/dashboard", nil)
	w := httptest.NewRecorder()
	ownerRoutes.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner dashboard: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		DataMode string `json:"data_mode"`
		Totals   struct {
			Energy *struct {
				Value string `json:"value"`
			} `json:"energy"`
			Cost *struct {
				Amount string `json:"amount"`
			} `json:"cost"`
		} `json:"totals"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.DataMode != "household" || body.Totals.Energy == nil || body.Totals.Energy.Value != "0.002500000000" || body.Totals.Cost == nil || body.Totals.Cost.Amount != "0.000500" {
		t.Fatalf("Hub interval did not reach priced owner dashboard: %+v", body)
	}
}
