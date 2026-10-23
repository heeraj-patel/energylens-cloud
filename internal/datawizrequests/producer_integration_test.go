package datawizrequests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/democost/testfixture"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawizrequests_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		conn.Close(ctx)
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return ctx, pool
}

func TestProducerSavedInputsAndRevisions(t *testing.T) {
	ctx, pool := testPool(t)
	var err error
	owner, home, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','60601','postal_code','user_entered')`, []any{home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'HVAC','hvac','{"brand":"Acme","full_model":"Heat-100","match_quality":"confirmed_exact"}')`, []any{appliance, home}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	p := Producer{DB: pool, Dir: dir}
	if count, err := p.RunOnce(ctx); err != nil || count != 14 {
		t.Fatalf("first pass: %d %v", count, err)
	}
	assertPayload := func(route string, want map[string]any) {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(dir, route, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("%s files: %v %v", route, paths, err)
		}
		body, err := os.ReadFile(paths[len(paths)-1])
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err = json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("%s %s: %v", route, key, got[key])
			}
		}
	}
	assertPayload("device", map[string]any{"manufacturer": "Acme", "model": "Heat-100"})
	assertPayload("resolve", map[string]any{"appliance_ref": "cloud:" + appliance, "appliance_version": float64(1), "device_kind": "real_non_metering", "category": "hvac"})
	assertPayload("zip", map[string]any{"zip": "60601"})
	if count, err := p.RunOnce(ctx); err != nil || count != 0 {
		t.Fatalf("repeat pass: %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET version=2,identity='{"brand":"Acme","full_model":"Heat-200"}' WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','02108','postal_code','user_entered')`, home); err != nil {
		t.Fatal(err)
	}
	if count, err := p.RunOnce(ctx); err != nil || count != 3 {
		t.Fatalf("updated pass: %d %v", count, err)
	}
	entry, found, err := p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.ApplianceVersion != 2 || entry.entry.Model != "Heat-200" {
		t.Fatalf("edited appliance resolve request: %+v %t %v", entry, found, err)
	}
	if paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json")); len(paths) != 2 {
		t.Fatalf("device revisions: %d", len(paths))
	}
	if paths, _ := filepath.Glob(filepath.Join(dir, "zip", "*.json")); len(paths) != 2 {
		t.Fatalf("ZIP revisions: %d", len(paths))
	}
	runID, observationID := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES($1,'device','{"manufacturer":"Acme","model":"Heat-200"}','realtime','complete')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at)
VALUES($1,$2,'device','device:acme:heat-200','energy_rating','annual','{"amount":"500","unit":"kWh/year"}','{"adapter":"test","url":"https://example.invalid/source","retrieved_at":"2026-10-12T00:00:00Z"}','approved',now())`, observationID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES('device','device:acme:heat-200','energy_rating','annual',$1)`, observationID); err != nil {
		t.Fatal(err)
	}
	facts, err := (datawizfacts.Store{DB: pool}).Current(ctx, "device", "device:acme:heat-200")
	if err != nil || len(facts) != 1 || facts[0].Field != "energy_rating" {
		t.Fatalf("approved current facts: %+v %v", facts, err)
	}
	if err = Queue(ctx, pool, appliance, "admin"); err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			count, e := p.drainQueue(ctx)
			if e != nil {
				results <- -1
				return
			}
			results <- count
		}()
	}
	if a, b := <-results, <-results; a+b != 1 || a < 0 || b < 0 {
		t.Fatalf("concurrent queue drains: %d %d", a, b)
	}
	if err = Queue(ctx, pool, appliance, "admin"); err != nil {
		t.Fatal(err)
	}
	if count, e := p.drainQueue(ctx); e != nil || count != 1 {
		t.Fatalf("replay queue: %d %v", count, e)
	}
	snapshotJSON, err := json.Marshal(testfixture.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET demo_cost_snapshot=$2 WHERE id=$1`, home, snapshotJSON); err != nil {
		t.Fatal(err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 10 {
		t.Fatalf("simulated initial wave: %d %v", count, e)
	}
	demos, err := inventory.DemoAppliances(testfixture.DemoSnapshot(), home)
	if err != nil {
		t.Fatal(err)
	}
	if len(demos) != 10 {
		t.Fatalf("demo appliances: %d", len(demos))
	}
	demoEntry, found, err := p.resolveEntryFor(ctx, demos[0].ID)
	if err != nil || !found || demoEntry.entry.ApplianceVersion != 1 {
		t.Fatalf("simulated resolve version: %+v %t %v", demoEntry, found, err)
	}
	if err = Queue(ctx, pool, demos[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if count, e := p.drainQueue(ctx); e != nil || count != 1 {
		t.Fatalf("simulated on-demand queue: %d %v", count, e)
	}
	hub, sensor, attachment := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_resolve_producer',$2,'test',$3,1)`, hub, make([]byte, 32), home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,$3,1,'meter','plug',ARRAY['power','energy'])`, sensor, home, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,2,'{}')`, appliance, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,2,now())`, attachment, home, sensor, appliance); err != nil {
		t.Fatal(err)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.DeviceKind != "real_metering" {
		t.Fatalf("metering attachment request: %+v %t %v", entry, found, err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("attachment change request: %d %v", count, e)
	}
	if _, err = pool.Exec(ctx, `UPDATE attachments SET ended_at=now(),version=2 WHERE id=$1`, attachment); err != nil {
		t.Fatal(err)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.DeviceKind != "real_non_metering" {
		t.Fatalf("closed metering attachment request: %+v %t %v", entry, found, err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("attachment closure request: %d %v", count, e)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET name='Renamed HVAC',version=3 WHERE id=$1`, appliance); err != nil {
		t.Fatal(err)
	}
	if count, e := p.emitNewResolutions(ctx); e != nil || count != 1 {
		t.Fatalf("name-only version bump did not requeue: %d %v", count, e)
	}
	entry, found, err = p.resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.ApplianceVersion != 3 {
		t.Fatalf("name-only version request: %+v %t %v", entry, found, err)
	}
}

func TestProducerDedupAndCatalogLinks(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home, home2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exactID, famID, wrongID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id) VALUES($1)`, owner)
	for _, h := range []string{home, home2} {
		exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'H','America/Chicago')`, h, owner)
		exec(`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','60601','postal_code','user_entered')`, h)
	}
	for _, p := range [][]string{{exactID, "Acme", "Heat-100", "confirmed_exact"}, {famID, "Acme", "Fam-1", "documented_variant_family"}, {wrongID, "Acme", "Other-9", "confirmed_exact"}} {
		exec(`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'hvac',$2,$3,'electricity',$4,'{}')`, p[0], p[1], p[2], p[3])
	}
	addFuel := func(home, name, model, fuel string, link *string) string {
		id := uuid.NewString()
		exec(`INSERT INTO appliances(id,home_id,name,category,catalog_product_id,identity) VALUES($1,$2,$3,'hvac',$4,$5)`, id, home, name, link,
			`{"brand":"Acme","full_model":"`+model+`","fuel":"`+fuel+`"}`)
		return id
	}
	add := func(home, name, model string, link *string) string {
		return addFuel(home, name, model, "electricity", link)
	}
	count := func(dir, route string) int {
		paths, _ := filepath.Glob(filepath.Join(dir, route, "*.json"))
		return len(paths)
	}
	dir := t.TempDir()
	p := Producer{DB: pool, Dir: dir}
	a1 := add(home, "A", "Same-1", nil)
	add(home2, "B", "SAME 1", nil)
	if _, err := p.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(dir, "device") != 1 || count(dir, "savings") != 1 || count(dir, "zip") != 1 {
		t.Fatalf("same model, brand and ZIP: device=%d savings=%d zip=%d", count(dir, "device"), count(dir, "savings"), count(dir, "zip"))
	}
	exec(`UPDATE appliances SET name='Renamed',room='Den',version=2 WHERE id=$1`, a1)
	if _, err := p.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(dir, "device") != 1 || count(dir, "savings") != 1 || count(dir, "zip") != 1 {
		t.Fatalf("rename emitted new files: device=%d savings=%d zip=%d", count(dir, "device"), count(dir, "savings"), count(dir, "zip"))
	}

	exactApp := add(home, "Exact", "heat 100", &exactID)
	famApp := add(home, "Family", "Fam-2", &famID)
	wrongApp := add(home, "Wrong", "Heat-100", &wrongID)
	gasApp := addFuel(home, "Gas", "heat 100", "natural_gas", &exactID)
	before := count(dir, "device")
	if _, err := p.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := count(dir, "device") - before; got != 2 {
		t.Fatalf("device files for family and mismatched legacy link: %d", got)
	}
	for _, c := range []struct {
		id, link, quality string
	}{{exactApp, exactID, "confirmed_exact"}, {famApp, famID, "documented_variant_family"}, {wrongApp, "", ""}, {gasApp, "", ""}} {
		entry, found, err := p.resolveEntryFor(ctx, c.id)
		if err != nil || !found || entry.entry.CatalogProductID != c.link || entry.entry.MatchQuality != c.quality {
			t.Fatalf("resolve link for %s: %+v %t %v", c.id, entry.entry, found, err)
		}
		body, _ := json.Marshal(entry.entry)
		if strings.Contains(string(body), `"catalog"`) {
			t.Fatalf("catalog block in %s", body)
		}
	}
}

func TestProducerExactLinkNeedsActiveFactToSkipDevice(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home := uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		`INSERT INTO users(id) VALUES('` + owner + `')`,
		`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES('` + home + `','` + owner + `','Home','America/Chicago')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	hash := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model    string
		withFact bool
		want     int
	}{{"Bare-1", false, 1}, {"Fact-1", true, 0}} {
		product, appliance := uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'heat_pump','Acme',$2,'electricity','confirmed_exact','{}')`, product, tc.model); err != nil {
			t.Fatal(err)
		}
		if tc.withFact {
			if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',20,'kWh','{}','{}','epa_exact','https://example.org/fact',$3,'reviewed_row_manifest','sources/example.json',now(),1,'{}')`, uuid.NewString(), product, hash); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES($1,$2,'HP','heat_pump',jsonb_build_object('brand','Acme','full_model',$3::text),$4)`, appliance, home, tc.model, product); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if _, err := (Producer{DB: pool, Dir: dir}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json"))
		found := 0
		for _, path := range paths {
			if body, _ := os.ReadFile(path); strings.Contains(string(body), tc.model) {
				found++
			}
		}
		if found != tc.want {
			t.Fatalf("%s device requests: got %d want %d", tc.model, found, tc.want)
		}
	}
}

func TestProducerOnlyAnnualEnergyCountsAsAFact(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home, product, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	hash := strings.Repeat("b", 64)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'H','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, []any{hash}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'boiler','Acme','GasBoil-1','natural_gas','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES(gen_random_uuid(),$1,$2,'standby_power',4,'W','{}','{}','epa_exact','https://example.org/fact',$2,'reviewed_row_manifest','m',now(),1,'{}')`, []any{product, hash}},
		{`INSERT INTO appliances(id,home_id,name,category,catalog_product_id,identity) VALUES($1,$2,'Boiler','boiler',$3,'{"brand":"Acme","full_model":"GasBoil-1","fuel":"natural_gas","match_quality":"confirmed_exact"}')`, []any{appliance, home, product}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if _, err := (Producer{DB: pool, Dir: dir}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	found := 0
	paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json"))
	for _, path := range paths {
		if body, _ := os.ReadFile(path); strings.Contains(string(body), "GasBoil-1") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("a standby_power fact must not count as an annual_energy fact: %d device requests", found)
	}
}

func TestProducerTrimLinkIsSent(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home, product, appliance := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'H','America/Chicago')`, []any{home, owner}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'refrigerator','KitchenAid','KRMF706EBS','electricity','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO appliances(id,home_id,name,category,catalog_product_id,identity) VALUES($1,$2,'Fridge','refrigerator',$3,'{"brand":"KitchenAid","full_model":"KRMF706XYZ","fuel":"electricity","match_quality":"documented_variant_family"}')`, []any{appliance, home, product}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	entry, found, err := (Producer{DB: pool, Dir: t.TempDir()}).resolveEntryFor(ctx, appliance)
	if err != nil || !found || entry.entry.CatalogProductID != product || entry.entry.MatchQuality != "documented_variant_family" {
		t.Fatalf("trim link in resolve entry: %+v %t %v", entry.entry, found, err)
	}
}

type scanCounter struct{ scans atomic.Int32 }

func (c *scanCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM appliances a JOIN homes h") && strings.Contains(data.SQL, "LEFT JOIN LATERAL") {
		c.scans.Add(1)
	}
	return ctx
}

func (c *scanCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDrainQueueScansInventoryOncePerBatch(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home := uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		`INSERT INTO users(id) VALUES('` + owner + `')`,
		`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES('` + home + `','` + owner + `','H','America/Chicago')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'A','light','{}')`, id, home); err != nil {
			t.Fatal(err)
		}
		if err := Queue(ctx, pool, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	counter := &scanCounter{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	dir := t.TempDir()
	if n, err := (Producer{DB: traced, Dir: dir}).drainQueue(ctx); err != nil || n != 5 {
		t.Fatalf("drain: %d %v", n, err)
	}
	if got := counter.scans.Load(); got != 1 {
		t.Fatalf("inventory scans per drain: %d", got)
	}
	if paths, _ := filepath.Glob(filepath.Join(dir, "resolve", "*.json")); len(paths) != 5 {
		t.Fatalf("resolve files: %d", len(paths))
	}
}

func TestProducerSkipsDeviceLookupOnlyForSameFuelFactLink(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home := uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		`INSERT INTO users(id) VALUES('` + owner + `')`,
		`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES('` + home + `','` + owner + `','Home','America/Chicago')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	hash := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model, productFuel, applianceFuel string
		want                              int
	}{{"Gas-Same-1", "natural_gas", "natural_gas", 0}, {"Gas-Mismatch-1", "electricity", "natural_gas", 1}, {"Elec-Mismatch-1", "natural_gas", "electricity", 1}} {
		product := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'dryer','Acme',$2,$3,'confirmed_exact','{}')`, product, tc.model, tc.productFuel); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',20,'kWh','{}','{}','epa_exact','https://example.org/fact',$3,'reviewed_row_manifest','sources/example.json',now(),1,'{}')`, uuid.NewString(), product, hash); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES($1,$2,'Dryer','dryer',jsonb_build_object('brand','Acme','full_model',$3::text,'fuel',$4::text,'match_quality','confirmed_exact'),$5)`, uuid.NewString(), home, tc.model, tc.applianceFuel, product); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if _, err := (Producer{DB: pool, Dir: dir}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json"))
		found := 0
		for _, path := range paths {
			if body, _ := os.ReadFile(path); strings.Contains(string(body), tc.model) {
				found++
			}
		}
		if found != tc.want {
			t.Fatalf("%s device requests: got %d want %d", tc.model, found, tc.want)
		}
	}
}

func TestProducerGasLinkWithoutFactsStillSendsDeviceLookup(t *testing.T) {
	ctx, pool := testPool(t)
	owner, home, product := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		`INSERT INTO users(id) VALUES('` + owner + `')`,
		`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES('` + home + `','` + owner + `','Home','America/Chicago')`,
		`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES('` + product + `','dryer','Acme','Gas-Bare-1','natural_gas','confirmed_exact','{}')`,
		`INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id) VALUES('` + uuid.NewString() + `','` + home + `','Dryer','dryer','{"brand":"Acme","full_model":"Gas-Bare-1","fuel":"natural_gas","match_quality":"confirmed_exact"}','` + product + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if _, err := (Producer{DB: pool, Dir: dir}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "device", "*.json"))
	found := 0
	for _, path := range paths {
		if body, _ := os.ReadFile(path); strings.Contains(string(body), "Gas-Bare-1") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("fact-less gas link device requests: got %d want 1", found)
	}
}
