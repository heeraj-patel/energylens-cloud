package householdenergy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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

func TestMeasuredCostReconcilesAndKeepsClassificationsUnknown(t *testing.T) {
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	buckets := makeBuckets(period{start, start.Add(time.Hour)}, "hour", []interval{
		{Start: start.Add(time.Minute), End: start.Add(2 * time.Minute), Wh: big.NewRat(1500, 1)},
	})
	cost := priceBuckets(buckets, marginalTariff{Rate: big.NewRat(20, 100), Currency: "USD"})
	if cost == nil || cost.Amount != "0.30" || buckets[0].Allocation.Cost.(money).Amount != cost.Amount || buckets[0].Allocation.UnclassifiedCost.(money).Amount != cost.Amount {
		t.Fatalf("measured energy cost did not reconcile: total=%+v buckets=%+v", cost, buckets)
	}
	if buckets[0].Allocation.EvidenceBasis != "mixed" || buckets[0].Allocation.UsefulCost != nil || buckets[0].Allocation.WasteCost != nil {
		t.Fatalf("unsupported cost classification was populated: %+v", buckets[0].Allocation)
	}
}

func TestAllocationChangesSnapshotWhenCostOrEvidenceBasisChanges(t *testing.T) {
	energy := &quantity{Value: "1.500000000000", Unit: "kWh"}
	measured := allocation{Energy: energy, Availability: "partial", EvidenceBasis: "measured"}
	priced := measured
	priced.Cost = money{Amount: "0.30", Currency: "USD"}
	priced.UnclassifiedCost = priced.Cost
	priced.EvidenceBasis = "mixed"
	if allocationSnapshotState(measured) == allocationSnapshotState(priced) {
		t.Fatal("pricing transition reused the same allocation snapshot state")
	}
	expired := measured
	if allocationSnapshotState(priced) == allocationSnapshotState(expired) {
		t.Fatal("tariff expiry transition reused the same allocation snapshot state")
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
	withoutTariffSnapshot := live["snapshot_id"]
	if code, replaced := getSample(dash, "owner"); code != 200 || replaced["data_mode"] != "household" || replaced["totals"].(map[string]any)["cost"] != nil {
		t.Fatalf("real evidence did not replace sample: %d %+v", code, replaced)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','owner bill',$2,false,$3)`, home.ID, now.Add(-10*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, priced := get(dash, "owner")
	if code != 200 {
		t.Fatalf("priced dashboard: %d %+v", code, priced)
	}
	pricedTotals := priced["totals"].(map[string]any)
	if pricedTotals["cost"].(map[string]any)["amount"] != "0.30" || pricedTotals["unclassified_cost"].(map[string]any)["amount"] != "0.30" || pricedTotals["useful_cost"] != nil || pricedTotals["waste_cost"] != nil || priced["evidence_basis"] != "mixed" {
		t.Fatalf("measured cost contract mismatch: %+v", priced)
	}
	if priced["snapshot_id"] == withoutTariffSnapshot {
		t.Fatal("snapshot did not change when a tariff revision was added")
	}
	code, series := get(trend, "owner")
	if code != 200 || series["snapshot_id"] != priced["snapshot_id"] {
		t.Fatalf("trend snapshot mismatch: %d %+v", code, series)
	}
	if series["totals"].(map[string]any)["energy"].(map[string]any)["value"] != totals["energy"].(map[string]any)["value"] {
		t.Fatal("dashboard and trend do not reconcile")
	}
	if series["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != pricedTotals["cost"].(map[string]any)["amount"] {
		t.Fatal("dashboard and trend cost do not reconcile")
	}
	// A correction must replace its earlier observation in both views. The
	// corrected value also changes the priced allocation and snapshot identity.
	correctedEnergy := 500.0
	correction := event
	correction.Revision = 2
	correction.EnergyWh = &correctedEnergy
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{correction}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, corrected := get(dash, "owner")
	if code != 200 || corrected["snapshot_id"] == priced["snapshot_id"] {
		t.Fatalf("correction did not change dashboard snapshot: %d %+v", code, corrected)
	}
	correctedTotals := corrected["totals"].(map[string]any)
	if correctedTotals["energy"].(map[string]any)["value"] != "0.500000000000" || correctedTotals["cost"].(map[string]any)["amount"] != "0.10" {
		t.Fatalf("corrected observation was not priced once: %+v", corrected)
	}
	code, correctedSeries := get(trend, "owner")
	if code != 200 || correctedSeries["snapshot_id"] != corrected["snapshot_id"] || !reflect.DeepEqual(correctedSeries["period"], corrected["period"]) || correctedSeries["scope"] != corrected["scope"] {
		t.Fatalf("corrected views diverged: %d %+v", code, correctedSeries)
	}
	seriesTotals := correctedSeries["totals"].(map[string]any)
	if seriesTotals["energy"].(map[string]any)["value"] != "0.500000000000" || seriesTotals["cost"].(map[string]any)["amount"] != "0.10" {
		t.Fatalf("corrected totals diverged: %+v", correctedSeries)
	}
	bucketEnergy, bucketCost := new(big.Rat), new(big.Rat)
	for _, raw := range correctedSeries["buckets"].([]any) {
		part := raw.(map[string]any)["allocation"].(map[string]any)
		if part["useful_energy"] != nil || part["useful_cost"] != nil || part["waste_energy"] != nil || part["waste_cost"] != nil {
			t.Fatalf("corrected bucket invented a classification: %+v", part)
		}
		if part["energy"] != nil {
			v, ok := new(big.Rat).SetString(part["energy"].(map[string]any)["value"].(string))
			if !ok {
				t.Fatalf("invalid bucket energy: %+v", part)
			}
			bucketEnergy.Add(bucketEnergy, v)
		}
		if part["cost"] != nil {
			v, ok := new(big.Rat).SetString(part["cost"].(map[string]any)["amount"].(string))
			if !ok {
				t.Fatalf("invalid bucket cost: %+v", part)
			}
			bucketCost.Add(bucketCost, v)
		}
	}
	if bucketEnergy.Cmp(big.NewRat(1, 2)) != 0 || bucketCost.Cmp(big.NewRat(1, 10)) != 0 {
		t.Fatalf("corrected buckets do not reconcile: energy=%s cost=%s", bucketEnergy, bucketCost)
	}
	// This correction crosses an hourly bucket boundary. Neither view can
	// allocate the interval to an hour, so neither may retain its old cost.
	straddlingStart := start.Add(-30 * time.Second)
	straddlingEnd := end.Add(-30 * time.Second)
	correction.Revision = 3
	correction.Start = &straddlingStart
	correction.End = &straddlingEnd
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{correction}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, unallocated := get(dash, "owner")
	if code != 200 || unallocated["snapshot_id"] == corrected["snapshot_id"] {
		t.Fatalf("measurement eligibility correction retained snapshot: %d %+v", code, unallocated)
	}
	code, unallocatedSeries := get(trend, "owner")
	if code != 200 || unallocatedSeries["snapshot_id"] != unallocated["snapshot_id"] || !reflect.DeepEqual(unallocatedSeries["period"], unallocated["period"]) || unallocatedSeries["scope"] != unallocated["scope"] {
		t.Fatalf("unallocated views diverged: %d %+v", code, unallocatedSeries)
	}
	for _, response := range []map[string]any{unallocated, unallocatedSeries} {
		unknown := response["totals"].(map[string]any)
		if unknown["energy"] != nil || unknown["cost"] != nil || unknown["unclassified_cost"] != nil || unknown["useful_energy"] != nil || unknown["useful_cost"] != nil || unknown["waste_energy"] != nil || unknown["waste_cost"] != nil || response["availability"] != "unavailable" {
			t.Fatalf("unallocatable measurement fabricated a total: %+v", response)
		}
	}
	for _, raw := range unallocatedSeries["buckets"].([]any) {
		part := raw.(map[string]any)["allocation"].(map[string]any)
		if part["energy"] != nil || part["cost"] != nil || part["useful_energy"] != nil || part["useful_cost"] != nil || part["waste_energy"] != nil || part["waste_cost"] != nil {
			t.Fatalf("unallocatable correction retained a bucket value: %+v", part)
		}
	}
	correction.Revision = 4
	correction.Start = &start
	correction.End = &end
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{correction}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, restored := get(dash, "owner")
	if code != 200 || restored["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != "0.10" {
		t.Fatalf("restored measurement was not priced: %d %+v", code, restored)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,2,'electricity',0.25,'kWh','USD','user_tariff','updated bill',$2,false,$3)`, home.ID, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, revised := get(dash, "owner")
	if code != 200 || revised["snapshot_id"] == restored["snapshot_id"] || revised["totals"].(map[string]any)["cost"] != nil {
		t.Fatalf("tariff revision did not invalidate snapshot or ambiguity was priced: %d %+v", code, revised)
	}
	code, revisedSeries := get(trend, "owner")
	if code != 200 || revisedSeries["snapshot_id"] != revised["snapshot_id"] || !reflect.DeepEqual(revisedSeries["period"], revised["period"]) || revisedSeries["scope"] != revised["scope"] {
		t.Fatalf("tariff ambiguity views diverged: %d %+v", code, revisedSeries)
	}
	for _, response := range []map[string]any{revised, revisedSeries} {
		total := response["totals"].(map[string]any)
		if total["cost"] != nil || total["unclassified_cost"] != nil || total["useful_energy"] != nil || total["useful_cost"] != nil || total["waste_energy"] != nil || total["waste_cost"] != nil {
			t.Fatalf("ambiguous tariff fabricated cost or classification: %+v", response)
		}
	}
	// The tariff expires at exactly 60*24 hours; test the strict freshness
	// boundary directly so DST cannot turn it into calendar-day arithmetic.
	staleHome, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "Stale tariff", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	boundary := now.Add(-60 * 24 * time.Hour)
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','expired bill',$2,false,$3)`, staleHome.ID, boundary, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The strict 1440-hour freshness window includes the instant before the
	// boundary and excludes the exact boundary.
	before := now.Add(-time.Microsecond)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	items := []interval{{Start: start, End: end, Wh: big.NewRat(1500, 1), Fuel: &fuel}}
	latestID, latestRevision, selected, err := readMarginalTariff(ctx, tx, staleHome.ID, before, items)
	_ = tx.Rollback(ctx)
	if err != nil || latestID == "" || latestRevision != 1 || selected == nil {
		t.Fatalf("tariff was not selected before 60-day boundary: id=%q revision=%d tariff=%+v err=%v", latestID, latestRevision, selected, err)
	}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	latestID, latestRevision, selected, err = readMarginalTariff(ctx, tx, staleHome.ID, now, items)
	_ = tx.Rollback(ctx)
	if err != nil || latestID == "" || latestRevision != 1 || selected != nil {
		t.Fatalf("expired tariff was selected at exact 60-day boundary: id=%q revision=%d tariff=%+v err=%v", latestID, latestRevision, selected, err)
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
