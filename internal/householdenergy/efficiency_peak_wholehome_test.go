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

// A home with a root utility meter and a dishwasher plug must still price peak from the plug.
func TestPostgresPeakBehindWholeHomeMeter(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "peak_wh_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, hub := uuid.NewString(), uuid.NewString(), uuid.NewString()
	meter, plug, meterApp, dishApp := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	meterAtt, plugAtt, revision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	since := now.Add(-5 * 24 * time.Hour)
	exec(`INSERT INTO users(id) VALUES($1)`, owner)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	exec(`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_peak_wh_test',$2,'test',$3,1)`, hub, make([]byte, 32), home)
	exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,1,'main-meter','utility_meter')`, meter, home, hub)
	exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,parent_sensor_id) VALUES($1,$2,$3,1,'dishwasher-plug','plug',$4)`, plug, home, hub, meter)
	for _, a := range [][]string{{meterApp, "Home", "other"}, {dishApp, "Dishwasher", "dishwasher"}} {
		exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,$3,$4,'{}')`, a[0], home, a[1], a[2])
		exec(`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, a[0], home)
	}
	exec(`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,'{"appliance_version":1,"values":{"standby_watts":"1"},"source":{"tier":"model"}}',now())`, "cloud:"+dishApp, make([]byte, 32))
	exec(`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5),($6,$2,$7,$8,1,$5)`, meterAtt, home, meter, meterApp, since, plugAtt, plug, dishApp)
	exec(`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.10,'kWh','USD','user_tariff','bill',$2,false,$3)`, home, since, since.Add(-24*time.Hour))
	if err = pool.QueryRow(ctx, `SELECT id::text FROM tariff_revisions WHERE home_id=$1`, home).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,1,'peak',0.20,$2,'16:00','21:00',$3::smallint[])`, revision, everyDay, allMonths)
	observe := func(sensor, att string, from time.Time, wh int) {
		t.Helper()
		batch, event := uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,1,$2,$3,$4)`, hub, batch, home, make([]byte, 32))
		exec(`INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,1,$2,1,$3,$4,'energy_interval',$5,$6)`, hub, event, home, sensor, make([]byte, 32), batch)
		exec(`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel) VALUES($1,1,$2,1,'energy_interval',$3,$4,$5,3600,$6,'electricity')`, hub, event, from, from.Add(time.Hour), wh, att)
	}
	day := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	observe(meter, meterAtt, day, 2000)
	observe(plug, plugAtt, day, 1000)
	s := Service{Pool: pool, Now: func() time.Time { return now }}
	eff, err := s.TrailingEfficiency(ctx, owner, home)
	if err != nil || eff == nil {
		t.Fatalf("trailing efficiency = %+v, %v", eff, err)
	}
	var found bool
	for _, d := range eff.Devices {
		if d.ApplianceID != dishApp {
			continue
		}
		found = true
		// 1 kWh moved from the +0.20 adder to the day's lowest (0) adder.
		if d.Peak == nil || d.Peak.Amount != "0.200000" || d.Basis.Peak != "measured" {
			t.Fatalf("dishwasher peak = %+v basis %+v", d.Peak, d.Basis)
		}
	}
	if !found || eff.PeakRatesOpportunity == nil || eff.PeakRatesBasis != "measured" {
		t.Fatalf("efficiency peak = %+v basis %q", eff.PeakRatesOpportunity, eff.PeakRatesBasis)
	}
}
