package householdenergy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/attachments"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalPeriodDST(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	p := periodFor(time.Date(2026, 3, 8, 18, 0, 0, 0, time.UTC), loc, "hour")
	if got := p.End.Sub(p.Start); got != 23*time.Hour {
		t.Fatalf("spring day length = %v", got)
	}
	p = periodFor(time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC), loc, "hour")
	if got := p.End.Sub(p.Start); got != 25*time.Hour {
		t.Fatalf("fall day length = %v", got)
	}
}

func TestAmbiguousMeterTopologyStaysUnknown(t *testing.T) {
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	p := period{start, start.Add(time.Hour)}
	items := []interval{
		{Start: start.Add(time.Minute), End: start.Add(3 * time.Minute), Wh: big.NewRat(1000, 1)},
		{Start: start.Add(2 * time.Minute), End: start.Add(4 * time.Minute), Wh: big.NewRat(1000, 1)},
	}
	b := makeBuckets(p, "hour", items)
	if len(b) != 1 || b[0].Allocation.Energy != nil || b[0].Allocation.Availability != "unavailable" {
		t.Fatalf("overlapping meters were summed: %+v", b)
	}
}

func TestSampleBucketsReconcileAndDiscloseSyntheticBasis(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	p := periodFor(time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC), loc, "hour")
	buckets := sampleBuckets(p, "hour")
	if len(buckets) != 24 {
		t.Fatalf("sample bucket count = %d", len(buckets))
	}
	total := sampleTotal(len(buckets))
	if total.Energy.Value != "24" || total.Cost.(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}).Amount != "4.80" {
		t.Fatalf("sample totals did not reconcile: %+v", total)
	}
	for _, b := range buckets {
		if b.Allocation.EvidenceBasis != "synthetic" || b.Allocation.UsefulEnergy != nil || b.Allocation.WasteCost != nil || b.Allocation.UnclassifiedEnergy.Value != "1" {
			t.Fatalf("unsupported classification or provenance: %+v", b.Allocation)
		}
	}
}

func TestOwnerLiveReadsAndPartialEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "household_energy_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "live-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "live-other"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	s := Service{Pool: pool, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string {
		if r.Header.Get("Authorization") == "Bearer other" {
			return other.ID
		}
		return owner.ID
	}, Now: func() time.Time { return now }}
	router := chi.NewRouter()
	router.Route("/v1", s.Routes)
	get := func(path, actor string) (int, map[string]any) {
		req := httptest.NewRequest("GET", path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var data map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &data)
		return w.Code, data
	}
	dash := "/v1/homes/" + home.ID + "/dashboard"
	trend := "/v1/homes/" + home.ID + "/energy-trends?granularity=hour"
	if code, _ := get(dash, ""); code != 401 {
		t.Fatalf("no auth: %d", code)
	}
	if code, _ := get(dash, "other"); code != 404 {
		t.Fatalf("other owner: %d", code)
	}
	code, empty := get(dash, "owner")
	if code != 200 || empty["data_mode"] != "household" || empty["availability"] != "unavailable" || empty["scope"] != "attributed_devices" {
		t.Fatalf("empty dashboard: %d %+v", code, empty)
	}
	if empty["totals"].(map[string]any)["energy"] != nil {
		t.Fatal("missing energy became zero")
	}
	s.Sample = &SampleProvider{AllowedUsers: map[string]bool{owner.ID: true}, AllowedHomes: map[string]bool{home.ID: true}}
	sampleRouter := chi.NewRouter()
	sampleRouter.Route("/v1", s.Routes)
	getSample := func(path, actor string) (int, map[string]any) {
		req := httptest.NewRequest("GET", path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		sampleRouter.ServeHTTP(w, req)
		var data map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &data)
		return w.Code, data
	}
	if code, _ := getSample(dash, ""); code != 401 {
		t.Fatalf("sample masked auth: %d", code)
	}
	if code, _ := getSample(dash, "other"); code != 404 {
		t.Fatalf("sample crossed owner: %d", code)
	}
	if code, sample := getSample(dash, "owner"); code != 200 || sample["data_mode"] != "sample" || sample["fixture_revision"] != SampleRevision || sample["evidence_basis"] != "synthetic" {
		t.Fatalf("missing sample response: %d %+v", code, sample)
	}
	var hub, sensor, appliance string
	if err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_live_energy_test',$1,'test',$2,1) RETURNING id`, make([]byte, 32), home.ID).Scan(&hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('live-energy-cert',$1,1,now()+interval '1 hour','test')`, hub); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities,created_at) VALUES($1,$2,1,'meter','plug',ARRAY['power'],$3) RETURNING id`, home.ID, hub, now.Add(-24*time.Hour)).Scan(&sensor); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Device','refrigerator','{}') RETURNING id`, home.ID).Scan(&appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home.ID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("live"))
	attach, err := (attachments.Store{Pool: pool}).Create(ctx, owner.ID, home.ID, attachments.CreateInput{SensorID: sensor, ApplianceID: appliance, IdentityRevision: 1, StartedAt: now.Add(-4 * time.Hour)}, 1, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	start, end := now.Add(-2*time.Hour), now.Add(-2*time.Hour+time.Minute)
	energy := 1500.0
	covered := int64(60)
	fuel, quality, source := "electricity", "metered", "verified_driver"
	event := telemetry.Event{Kind: "energy_interval", EventID: uuid.NewString(), Revision: 1, SensorID: sensor, Start: &start, End: &end, EnergyWh: &energy, CoveredSeconds: &covered, AttachmentRevisionID: &attach.ID, Fuel: &fuel, Quality: &quality, Source: &source}
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{event}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, live := get(dash, "owner")
	if code != 200 {
		t.Fatalf("live dashboard: %d %+v", code, live)
	}
	totals := live["totals"].(map[string]any)
	if live["availability"] != "partial" || totals["energy"].(map[string]any)["value"] != "1.500000000000" || totals["cost"] != nil || totals["waste_energy"] != nil {
		t.Fatalf("unsupported value fabricated: %+v", live)
	}
	if live["snapshot_id"] == empty["snapshot_id"] {
		t.Fatal("snapshot unchanged after new evidence")
	}
	if code, replaced := getSample(dash, "owner"); code != 200 || replaced["data_mode"] != "household" || replaced["totals"].(map[string]any)["cost"] != nil {
		t.Fatalf("real evidence did not replace sample: %d %+v", code, replaced)
	}
	code, series := get(trend, "owner")
	if code != 200 || series["snapshot_id"] != live["snapshot_id"] {
		t.Fatalf("trend snapshot mismatch: %d %+v", code, series)
	}
	if series["totals"].(map[string]any)["energy"].(map[string]any)["value"] != totals["energy"].(map[string]any)["value"] {
		t.Fatal("dashboard and trend do not reconcile")
	}
	if code, _ := get("/v1/homes/"+home.ID+"/energy-trends?granularity=invalid", "owner"); code != 400 {
		t.Fatalf("invalid granularity: %d", code)
	}
	now = now.AddDate(0, 0, 1)
	code, stale := get(dash, "owner")
	if code != 200 || stale["totals"].(map[string]any)["energy"] != nil || stale["as_of"] != nil {
		t.Fatalf("prior-day evidence leaked into new period: %d %+v", code, stale)
	}
}
