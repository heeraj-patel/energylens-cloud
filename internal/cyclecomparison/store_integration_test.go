package cyclecomparison

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresExactDishwasherCycle(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "cycle_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	owner, other, home, hub, sensor, appliance, attachment, factID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-2 * time.Hour)
	end := start.Add(90 * time.Minute)
	exec(`INSERT INTO users(id) VALUES($1),($2)`, owner, other)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	exec(`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_cycle_test',$2,'test',$3,2)`, hub, make([]byte, 32), home)
	exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'dishwasher-plug','plug')`, sensor, home, hub)
	identity := `{"brand":"Acme","full_model":"DW-123","variant":"US-120V","fuel":"electricity","match_quality":"confirmed_exact"}`
	exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dishwasher','dishwasher',$3)`, appliance, home, identity)
	exec(`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,$3)`, appliance, home, identity)
	exec(`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5)`, attachment, home, sensor, appliance, start.Add(-time.Hour))
	exec(`INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$2,$3,'test',1)`, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64))
	exec(`INSERT INTO reviewed_usage_facts(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json) VALUES($1,$2,'dishwasher','Acme','DW123','electricity','US-120V','confirmed_exact','DW123','DW123','cycle_energy',0.7,'kWh/cycle','DOE Normal','Normal with heated dry','{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.test/dw.pdf',$3,0,$4,'reviewer',$5,'test.json','{}')`, factID, strings.Repeat("a", 64), strings.Repeat("d", 64), strings.Repeat("e", 64), now)
	in := Request{Start: start, End: end, AttachmentID: attachment, FactID: factID, ConfirmedTestBasis: "DOE Normal", ConfirmedCycleDefinition: "Normal with heated dry", DedicatedNoOtherLoads: true}
	s := Store{Pool: pool}
	check := func(want string) {
		t.Helper()
		got, e := s.Compare(ctx, owner, home, appliance, in, now)
		if e != nil {
			t.Fatal(e)
		}
		if got.Status != want && got.Reason != want {
			t.Fatalf("got %+v, want %s", got, want)
		}
		if want == "available" {
			if got.MeasuredKWh == nil || *got.MeasuredKWh != 0.8 || got.DifferenceKWh == nil || *got.DifferenceKWh < 0.099999 || *got.DifferenceKWh > 0.100001 {
				t.Fatalf("values: %+v", got)
			}
		} else if got.MeasuredKWh != nil || got.DifferenceKWh != nil || got.LabReferenceKWhPerCycle != nil {
			t.Fatalf("unavailable leaked values: %+v", got)
		}
	}
	if _, e := s.Compare(ctx, other, home, appliance, in, now); !errors.Is(e, ErrNotFound) {
		t.Fatalf("other owner: %v", e)
	}
	check("raw_interval_coverage_or_scope_invalid")
	batch := uuid.NewString()
	event1, event2 := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,2,$2,$3,$4)`, hub, batch, home, make([]byte, 32))
	interval := func(event string, revision int, startAt, endAt time.Time, wh float64) {
		t.Helper()
		exec(`INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,2,$2,$3,$4,$5,'energy_interval',$6,$7)`, hub, event, revision, home, sensor, make([]byte, 32), batch)
		exec(`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel,quality,source) VALUES($1,2,$2,$3,'energy_interval',$4,$5,$6,$7,$8,'electricity','metered','verified_driver')`, hub, event, revision, startAt, endAt, wh, int(endAt.Sub(startAt).Seconds()), attachment)
	}
	interval(event1, 1, start, start.Add(45*time.Minute), 300)
	check("raw_interval_coverage_or_scope_invalid")
	interval(event2, 1, start.Add(46*time.Minute), end, 500)
	check("raw_interval_coverage_or_scope_invalid")
	// Corrected latest revision closes the one-minute gap; old revision must
	// not remain visible and cause an overlap.
	interval(event2, 2, start.Add(45*time.Minute), end, 500)
	check("available")
	interval(event2, 3, start.Add(44*time.Minute), end, 500)
	check("raw_interval_coverage_or_scope_invalid")
	interval(event2, 4, start.Add(45*time.Minute), end, 500)
	gapID := uuid.NewString()
	exec(`INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,2,$2,1,$3,$4,'coverage_gap',$5,$6)`, hub, gapID, home, sensor, make([]byte, 32), batch)
	exec(`INSERT INTO coverage_gaps(hub_id,ownership_epoch,event_id,revision,first_at,last_at,reason,dropped_count) VALUES($1,2,$2,1,$3,$4,'offline',1)`, hub, gapID, start.Add(20*time.Minute), start.Add(21*time.Minute))
	check("coverage_gap_reported")
	exec(`DELETE FROM coverage_gaps WHERE event_id=$1`, gapID)
	check("available")
	in.DedicatedNoOtherLoads = false
	check("dedicated_use_unconfirmed")
	in.DedicatedNoOtherLoads = true
	in.ConfirmedCycleDefinition = "Quick"
	check("reviewed_basis_not_confirmed")
	in.ConfirmedCycleDefinition = "Normal with heated dry"
	in.FactID = uuid.NewString()
	check("reviewed_basis_not_confirmed")
	in.FactID = factID
	// A newer appliance identity revision invalidates the old attachment.
	exec(`UPDATE appliances SET version=2 WHERE id=$1`, appliance)
	check("attachment_not_dedicated_for_window")
	exec(`UPDATE appliances SET version=1 WHERE id=$1`, appliance)
	// The importer rejects duplicates; the read path independently refuses
	// ambiguity if a legacy or manually repaired database contains two facts.
	exec(`DROP INDEX reviewed_usage_facts_one_identity_idx`)
	exec(`INSERT INTO reviewed_usage_facts SELECT $1,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,dataset_id,row_identifier,field_name,metadata_sha256,review_sha256,reviewer,reviewed_at,source_manifest,fact_json,imported_at FROM reviewed_usage_facts WHERE id=$2`, uuid.NewString(), factID)
	check("reviewed_fact_missing_or_ambiguous")
	exec(`UPDATE homes SET owner_user_id=$1 WHERE id=$2`, other, home)
	if _, e := s.Compare(ctx, owner, home, appliance, in, now); !errors.Is(e, ErrNotFound) {
		t.Fatalf("old owner after transfer: %v", e)
	}
}
