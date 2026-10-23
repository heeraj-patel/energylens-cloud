package householdenergy

import (
	"context"
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

func habitFixture(t *testing.T) (componentInput, time.Time) {
	t.Helper()
	loc := nyLoc(t)
	start := time.Date(2026, 10, 14, 6, 0, 0, 0, loc)
	return nightFixture(t, start, 96, splitSpec{StandbyWatts: 0.5}, loc), start
}

func cycle(id string, start time.Time, measured, reference string) habitCycle {
	return habitCycle{ApplianceID: id, Start: start, End: start.Add(90 * time.Minute), MeasuredKWh: mustDecimal(measured), ReferenceKWh: mustDecimal(reference)}
}

func TestHabitsPricesMeasuredOverReferenceTimesCycles(t *testing.T) {
	in, start := habitFixture(t)
	// Two cycles each 0.5 kWh over reference at 0.10 USD/kWh.
	in.Habits = []habitCycle{cycle("dyson", start.Add(time.Hour), "1.2", "0.7"), cycle("dyson", start.Add(10*time.Hour), "1.2", "0.7")}
	got := habitsComponent(in).Devices["dyson"]
	if got.Amount == nil || got.Amount.Cmp(mustDecimal("0.1")) != 0 || got.Basis != "measured" {
		t.Fatalf("habits = %+v, want 0.1 measured", got)
	}
	eff := assembleMeasuredEfficiency(in)
	d := eff.Devices[0]
	if d.Habits == nil || d.Habits.Amount != "0.100000" || d.Basis.Habits != "measured" {
		t.Fatalf("device habits = %+v basis %+v", d.Habits, d.Basis)
	}
	if eff.HabitsOpportunity == nil || *eff.HabitsOpportunity != *d.Habits || eff.HabitsBasis != "measured" {
		t.Fatalf("home habits = %+v %s", eff.HabitsOpportunity, eff.HabitsBasis)
	}
}

func TestHabitsUsesAverageAcrossCyclesAndFloorsAtZero(t *testing.T) {
	in, start := habitFixture(t)
	// Average 0.8 kWh vs reference 0.7 over two cycles: 0.2 kWh x 0.10.
	in.Habits = []habitCycle{cycle("dyson", start.Add(time.Hour), "1.0", "0.7"), cycle("dyson", start.Add(10*time.Hour), "0.6", "0.7")}
	if got := habitsComponent(in).Devices["dyson"].Amount; got == nil || got.Cmp(mustDecimal("0.02")) != 0 {
		t.Fatalf("habits = %v, want 0.02", got)
	}
	in.Habits = []habitCycle{cycle("dyson", start.Add(time.Hour), "0.5", "0.7")}
	got := habitsComponent(in).Devices["dyson"]
	if got.Amount == nil || got.Amount.Sign() != 0 || got.Basis != "measured" {
		t.Fatalf("below reference = %+v, want 0 measured", got)
	}
	if d := assembleMeasuredEfficiency(in).Devices[0]; d.Habits == nil || d.Habits.Amount != "0.000000" {
		t.Fatalf("floored device habits = %+v", d.Habits)
	}
}

func TestHabitsCountsAnOverlappingSavedCycleOnce(t *testing.T) {
	in, start := habitFixture(t)
	in.Habits = []habitCycle{cycle("dyson", start.Add(time.Hour), "1.2", "0.7"), cycle("dyson", start.Add(time.Hour), "1.2", "0.7"), cycle("dyson", start.Add(2*time.Hour), "1.2", "0.7")}
	if got := habitsComponent(in).Devices["dyson"].Amount; got == nil || got.Cmp(mustDecimal("0.05")) != 0 {
		t.Fatalf("habits = %v, want 0.05", got)
	}
}

func TestHabitsWithoutBothInputsIsInsufficient(t *testing.T) {
	in, start := habitFixture(t)
	if res := habitsComponent(in); res.Basis != "insufficient_data" || len(res.Devices) != 0 {
		t.Fatalf("no cycles = %+v", res)
	}
	eff := assembleMeasuredEfficiency(in)
	if eff.HabitsOpportunity != nil || eff.HabitsBasis != "insufficient_data" || eff.Devices[0].Habits != nil || eff.Devices[0].Basis.Habits != "insufficient_data" {
		t.Fatalf("no cycles efficiency = %+v", eff)
	}
	in.Habits = []habitCycle{cycle("dyson", start.Add(time.Hour), "1.2", "")}
	in.Habits[0].ReferenceKWh = nil
	if res := habitsComponent(in); len(res.Devices) != 0 {
		t.Fatalf("no reference = %+v", res)
	}
	in.Habits = []habitCycle{cycle("other", start.Add(time.Hour), "1.2", "0.7")}
	if res := habitsComponent(in); len(res.Devices) != 0 {
		t.Fatalf("unmetered device counted = %+v", res)
	}
	if res := habitsComponent(componentInput{Habits: in.Habits}); res.Basis != "insufficient_data" || len(res.Devices) != 0 {
		t.Fatalf("no tariff = %+v", res)
	}
}

func TestPostgresHabitsFromSavedCycleEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "habits_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, other, home, hub, sensor, appliance, attachment := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-3 * time.Hour)
	exec(`INSERT INTO users(id) VALUES($1),($2)`, owner, other)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	exec(`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_habits_test',$2,'test',$3,1)`, hub, make([]byte, 32), home)
	exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,1,'dishwasher-plug','plug')`, sensor, home, hub)
	exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Dishwasher','dishwasher','{}')`, appliance, home)
	exec(`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home)
	exec(`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5)`, attachment, home, sensor, appliance, start.Add(-time.Hour))
	exec(`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','owner bill',$2,false,$3)`, home, now.Add(-10*24*time.Hour), now.Add(-40*24*time.Hour))
	for i := 0; i < 6; i++ {
		batch, event := uuid.NewString(), uuid.NewString()
		from := start.Add(time.Duration(i) * 10 * time.Minute)
		exec(`INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,1,$2,$3,$4)`, hub, batch, home, make([]byte, 32))
		exec(`INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,1,$2,1,$3,$4,'energy_interval',$5,$6)`, hub, event, home, sensor, make([]byte, 32), batch)
		exec(`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel) VALUES($1,1,$2,1,'energy_interval',$3,$4,200,600,$5,'electricity')`, hub, event, from, from.Add(10*time.Minute), attachment)
	}
	// Evidence rows are inserted after the observations so the interval trigger leaves them valid.
	evidence := func(user string, from time.Time, measured float64, invalid bool, expires time.Time) {
		t.Helper()
		var reason any
		if invalid {
			reason = "meter_evidence_changed"
		}
		exec(`INSERT INTO dishwasher_cycle_evidence(owner_user_id,home_id,appliance_id,appliance_version,attachment_id,sensor_id,hub_id,ownership_epoch,start_at,end_at,dedicated_no_other_loads,fact_id,fact_publication_sha256,fact_review_sha256,fact_source_sha256,confirmed_test_basis,confirmed_cycle_definition,measured_kwh,reference_kwh_per_cycle,interval_refs,idempotency_key,request_hash,invalidated_at,invalidation_reason,expires_at) VALUES($1,$2,$3,1,$4,$5,$6,1,$7,$8,true,$9,$10,$10,$10,'DOE Normal','Normal',$11,0.7,'[{"event_id":"e","revision":1}]',$12,$13,CASE WHEN $14::text IS NULL THEN NULL ELSE now() END,$14::text,$15)`,
			user, home, appliance, attachment, sensor, hub, from, from.Add(time.Hour), uuid.NewString(), strings.Repeat("a", 64), measured, uuid.NewString(), make([]byte, 32), reason, expires)
	}
	later := now.Add(24 * time.Hour)
	evidence(owner, start, 1.2, false, later)
	evidence(owner, now.Add(-6*time.Hour), 3.0, true, later)
	evidence(owner, now.Add(-7*time.Hour), 3.0, false, now.Add(-time.Minute))
	evidence(other, now.Add(-8*time.Hour), 3.0, false, later)
	evidence(owner, now.Add(-45*24*time.Hour), 3.0, false, later)
	s := Service{Pool: pool, Now: func() time.Time { return now }}
	eff, err := s.TrailingEfficiency(ctx, owner, home)
	if err != nil || eff == nil {
		t.Fatalf("trailing efficiency = %+v, %v", eff, err)
	}
	var found bool
	for _, d := range eff.Devices {
		if d.ApplianceID != appliance {
			continue
		}
		found = true
		// One valid cycle: (1.2 - 0.7) kWh x 0.20 USD/kWh.
		if d.Habits == nil || d.Habits.Amount != "0.100000" || d.Basis.Habits != "measured" {
			t.Fatalf("device habits = %+v basis %+v", d.Habits, d.Basis)
		}
	}
	if !found || eff.HabitsOpportunity == nil || eff.HabitsOpportunity.Amount != "0.100000" || eff.HabitsBasis != "measured" {
		t.Fatalf("efficiency = %+v", eff)
	}
}
