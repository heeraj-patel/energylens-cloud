package datawizrequests

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type requeueFixture struct {
	ctx  context.Context
	pool *pgxpool.Pool
	home string
	q    Requeuer
	now  time.Time
}

func newRequeueFixture(t *testing.T) requeueFixture {
	ctx, pool := testPool(t)
	owner, home := uuid.NewString(), uuid.NewString()
	for _, s := range []struct {
		q string
		a []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC')`, []any{home, owner}},
		{`INSERT INTO datawiz_source_state(fingerprint,updated_at) VALUES($1,now())`, []any{strings.Repeat("a", 64)}},
	} {
		if _, err := pool.Exec(ctx, s.q, s.a...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	return requeueFixture{ctx: ctx, pool: pool, home: home, q: Requeuer{DB: pool, Now: func() time.Time { return now }}, now: now}
}

func (f requeueFixture) appliance(t *testing.T, catalogID *string, identity string, record map[string]any, age time.Duration) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO appliances(id,home_id,name,category,identity,catalog_product_id,updated_at) VALUES($1,$2,'Device','light',$3,$4,$5)`, id, f.home, identity, catalogID, f.now.Add(-100*day)); err != nil {
		t.Fatal(err)
	}
	record["appliance_version"] = 1
	if _, ok := record["schema_version"]; !ok {
		record["schema_version"] = schemaV2
	}
	if _, ok := record["source_fingerprint"]; !ok {
		record["source_fingerprint"] = strings.Repeat("a", 64)
	}
	raw, _ := json.Marshal(record)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,$4)`, "cloud:"+id, make([]byte, 32), raw, f.now.Add(-age)); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f requeueFixture) attempt(t *testing.T, id, reason string, ago time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO datawiz_resolve_attempts(appliance_id,home_id,reason,queued_at) VALUES($1,$2,$3,$4)`, id, f.home, reason, f.now.Add(-ago)); err != nil {
		t.Fatal(err)
	}
}

func (f requeueFixture) queued(t *testing.T, id string) string {
	t.Helper()
	var reason string
	err := f.pool.QueryRow(f.ctx, `SELECT COALESCE((SELECT reason FROM datawiz_resolve_queue WHERE appliance_id=$1),'')`, id).Scan(&reason)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func (f requeueFixture) run(t *testing.T) int {
	t.Helper()
	n, err := f.q.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func resolved(tier string, extra map[string]any) map[string]any {
	source := map[string]any{"tier": tier, "adapter": "x"}
	values := map[string]any{"waste_basis": "standby"}
	for k, v := range extra {
		switch k {
		case "waste_basis":
			values[k] = v
		case "source_fingerprint", "schema_version":
		default:
			source[k] = v
		}
	}
	r := map[string]any{"status": "resolved", "source": source, "values": values}
	for _, k := range []string{"source_fingerprint", "schema_version"} {
		if v, ok := extra[k]; ok {
			r[k] = v
		}
	}
	return r
}

const plainIdentity = `{"brand":"Acme","full_model":"A1","fuel":"electricity","match_quality":"unresolved"}`

func TestRequeueTriggersAndGaps(t *testing.T) {
	f := newRequeueFixture(t)
	oem := func(extra map[string]any) map[string]any { return resolved("oem", extra) }

	lowTier := f.appliance(t, nil, plainIdentity, resolved("category_typical", nil), 8*day)
	lowTierYoung := f.appliance(t, nil, plainIdentity, resolved("category_typical", nil), 6*day)
	unresolved := f.appliance(t, nil, plainIdentity, map[string]any{"status": "unresolved", "source": nil, "values": nil}, 8*day)
	doubled := f.appliance(t, nil, plainIdentity, resolved("category_typical", nil), 20*day)
	f.attempt(t, doubled, "low_tier", 10*day)
	floor := f.appliance(t, nil, plainIdentity, oem(map[string]any{"waste_basis": "floor"}), 31*day)
	floorYoung := f.appliance(t, nil, plainIdentity, oem(map[string]any{"waste_basis": "floor"}), 29*day)
	stale := f.appliance(t, nil, plainIdentity, oem(nil), 31*day)
	staleRecentAttempt := f.appliance(t, nil, plainIdentity, oem(nil), 31*day)
	f.attempt(t, staleRecentAttempt, "stale", 5*day)
	fresh := f.appliance(t, nil, plainIdentity, oem(nil), day)
	fingerprint := f.appliance(t, nil, plainIdentity, oem(map[string]any{"source_fingerprint": strings.Repeat("b", 64)}), day)
	fingerprintRecent := f.appliance(t, nil, plainIdentity, oem(map[string]any{"source_fingerprint": strings.Repeat("b", 64)}), day)
	f.attempt(t, fingerprintRecent, "source_config", time.Hour)
	category := f.appliance(t, nil, plainIdentity, resolved("category_typical", map[string]any{"dataset_id": "energy_categories:0"}), day)
	v1 := f.appliance(t, nil, plainIdentity, map[string]any{"schema_version": "datawiz.energy-resolution.v1", "status": "resolved", "source": map[string]any{"tier": "oem"}, "values": map[string]any{}, "source_fingerprint": strings.Repeat("a", 64)}, day)

	if n := f.run(t); n != 6 {
		t.Fatalf("queued %d", n)
	}
	want := map[string]string{lowTier: "low_tier", unresolved: "low_tier", doubled: "", floor: "low_tier", stale: "stale",
		fingerprint: "source_config", category: "source_config", lowTierYoung: "", floorYoung: "", staleRecentAttempt: "",
		fresh: "", fingerprintRecent: "", v1: ""}
	for id, reason := range want {
		if got := f.queued(t, id); got != reason {
			t.Errorf("%s: queued %q want %q", id, got, reason)
		}
	}
	var attempts int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM datawiz_resolve_attempts WHERE queued_at=(SELECT queued_at FROM datawiz_resolve_queue WHERE appliance_id=$1)`, lowTier).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempt recorded: %d %v", attempts, err)
	}
	if n := f.run(t); n != 0 {
		t.Fatalf("second pass queued %d, rows already queued must be skipped", n)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM datawiz_resolve_attempts WHERE appliance_id=$1`, doubled); err != nil {
		t.Fatal(err)
	}
	f.attempt(t, doubled, "low_tier", 15*day)
	if n := f.run(t); n != 1 || f.queued(t, doubled) != "low_tier" {
		t.Fatalf("doubled gap of 14 days elapsed: %d", n)
	}
}

func TestRequeueCatalogChange(t *testing.T) {
	f := newRequeueFixture(t)
	hash := strings.Repeat("c", 64)
	product, fact := uuid.NewString(), uuid.NewString()
	for _, s := range []struct {
		q string
		a []any
	}{
		{`INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, []any{hash}},
		{`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'light','Acme','A1','electricity','confirmed_exact','{}')`, []any{product}},
		{`INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',20,'kWh','{}','{}','epa_exact','https://example.org/f',$3,'reviewed_row_manifest','s.json',now(),2,'{}')`, []any{fact, product, hash}},
	} {
		if _, err := f.pool.Exec(f.ctx, s.q, s.a...); err != nil {
			t.Fatal(err)
		}
	}
	linked := `{"brand":"Acme","full_model":"A1","fuel":"electricity","match_quality":"confirmed_exact"}`
	current := resolved("catalog", map[string]any{"record_key": "catalog:" + product + ":2", "scope": "exact"})
	old := resolved("catalog", map[string]any{"record_key": "catalog:" + product + ":1", "scope": "exact"})
	upToDate := f.appliance(t, &product, linked, current, day)
	newerFact := f.appliance(t, &product, linked, old, day)
	typicalNowLinked := f.appliance(t, &product, linked, resolved("category_typical", nil), day)
	linkGone := f.appliance(t, nil, plainIdentity, current, day)
	edited := f.appliance(t, &product, `{"brand":"Other","full_model":"Z9","fuel":"electricity","match_quality":"confirmed_exact"}`, current, day)
	recentAttempt := f.appliance(t, &product, linked, old, day)
	f.attempt(t, recentAttempt, "catalog_change", time.Hour)
	if n := f.run(t); n != 4 {
		t.Fatalf("queued %d", n)
	}
	for id, want := range map[string]string{upToDate: "", newerFact: "catalog_change", typicalNowLinked: "catalog_change", linkGone: "catalog_change", edited: "catalog_change", recentAttempt: ""} {
		if got := f.queued(t, id); got != want {
			t.Errorf("%s: queued %q want %q", id, got, want)
		}
	}
}

func TestRequeueCapAndExclusions(t *testing.T) {
	f := newRequeueFixture(t)
	for i := 0; i < requeueLimit+5; i++ {
		f.appliance(t, nil, plainIdentity, resolved("category_typical", nil), time.Duration(40+i)*day)
	}
	archived := f.appliance(t, nil, plainIdentity, resolved("category_typical", nil), 90*day)
	if _, err := f.pool.Exec(f.ctx, `UPDATE appliances SET archived_at=now() WHERE id=$1`, archived); err != nil {
		t.Fatal(err)
	}
	noRecord := uuid.NewString()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'N','light','{}')`, noRecord, f.home); err != nil {
		t.Fatal(err)
	}
	if n := f.run(t); n != requeueLimit {
		t.Fatalf("queued %d", n)
	}
	if f.queued(t, archived) != "" || f.queued(t, noRecord) != "" {
		t.Fatal("archived or recordless appliance queued")
	}
	var youngest int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM datawiz_resolve_queue q JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||q.appliance_id::text WHERE r.resolved_at > $1`, f.now.Add(-time.Duration(44*24+12)*time.Hour)).Scan(&youngest); err != nil || youngest != 0 {
		t.Fatalf("oldest records must go first: %d %v", youngest, err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO datawiz_resolve_attempts(appliance_id,home_id,reason,queued_at) VALUES($1,$2,'stale',$3)`, noRecord, f.home, f.now.Add(-91*day)); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	var kept int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM datawiz_resolve_attempts WHERE appliance_id=$1`, noRecord).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("attempts older than 90 days kept: %d %v", kept, err)
	}
}

func TestRequeueSchemaWave(t *testing.T) {
	f := newRequeueFixture(t)
	v1 := f.appliance(t, nil, plainIdentity, map[string]any{"schema_version": "datawiz.energy-resolution.v1", "status": "resolved", "source": map[string]any{"tier": "oem"}, "values": map[string]any{}}, time.Hour)
	v2 := f.appliance(t, nil, plainIdentity, resolved("oem", nil), time.Hour)
	f.attempt(t, v1, "schema", time.Minute)
	n, err := f.q.SchemaWave(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("wave: %d %v", n, err)
	}
	if f.queued(t, v1) != "schema" || f.queued(t, v2) != "" {
		t.Fatal("wave queued the wrong rows")
	}
	if n, err = f.q.SchemaWave(f.ctx); err != nil || n != 0 {
		t.Fatalf("rerun: %d %v", n, err)
	}
}
