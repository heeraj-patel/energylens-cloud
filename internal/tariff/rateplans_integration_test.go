package tariff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type planDB struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	owner string
}

func newPlanDB(t *testing.T) planDB {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "rate_plan_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	owner, err := (account.PGStore{Pool: pool}).Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "rate-plan-owner"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	return planDB{t, ctx, pool, owner.ID}
}

func (d planDB) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.pool.Exec(d.ctx, sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

func (d planDB) home(zip string) string {
	d.t.Helper()
	h, err := (account.PGStore{Pool: d.pool}).Create(d.ctx, d.owner, account.HomeCreate{Name: "House " + zip, Timezone: "America/Los_Angeles",
		Location: account.Location{CountryCode: "US", PostalCode: zip, Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		d.t.Fatal(err)
	}
	return h.ID
}

func (d planDB) utilityZIP(zip, eiaid, name, resRate string) {
	d.t.Helper()
	d.exec(`INSERT INTO oedi_utility_zips(edition_year,file_kind,source_row,zip,eiaid,utility_name,state,service_type,ownership,res_rate,dataset_url,published_on,source_url,source_sha256,retrieved_at)
 VALUES(2024,'iou',(SELECT COUNT(*)+1 FROM oedi_utility_zips),$1,$2,$3,'CA','Bundled','Investor Owned',$4::numeric,'https://d.test',now(),'https://s.test',$5,now())`, zip, eiaid, name, resRate, strings.Repeat("a", 64))
}

type testPlan struct {
	eiaid, key, name string
	variant          *string
	standard         bool
	tiers            any
	fixed, per       any
	checked          time.Time
	version          int
	periods          [][]any
}

func (d planDB) plan(p testPlan) string {
	d.t.Helper()
	kind := "official_tariff"
	if strings.HasPrefix(p.key, "urdb:") {
		kind = "urdb"
	}
	if p.version == 0 {
		p.version = 1
	}
	var id string
	err := d.pool.QueryRow(d.ctx, `INSERT INTO utility_rate_plans(eiaid,utility_name,sector,plan_key,version,plan_name,variant,is_standard,base_tiers,base_label,
 fixed_charge,fixed_charge_per,currency,effective_from,source_kind,source_url,source_file_url,source_sha256,source_notes,retrieved_at,checked_at)
 VALUES($1,'Utility','residential',$2,$3,$4,$5,$6,$7,'winter off-peak',$8::numeric,$9,'USD','2026-01-01',$10,'https://tariff.test/'||$2,'https://tariff.test/file',$11,'["whole-cent prices"]',$12,$12) RETURNING id::text`,
		p.eiaid, p.key, p.version, p.name, p.variant, p.standard, p.tiers, p.fixed, p.per, kind, strings.Repeat("c", 63)+string(rune('0'+p.version)), p.checked).Scan(&id)
	if err != nil {
		d.t.Fatal(err)
	}
	for i, row := range p.periods {
		d.exec(`INSERT INTO utility_rate_plan_periods(plan_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,$2,$3,$4::numeric,$5,$6::time,$7::time,$8::smallint[])`,
			append([]any{id, i + 1}, row...)...)
	}
	return id
}

type revisionRow struct {
	id, rate, sourceRef string
	planID, chosenBy    *string
	sourceAsOf          time.Time
	periods             int
}

func (d planDB) revisions(home string) []revisionRow {
	d.t.Helper()
	rows, err := d.pool.Query(d.ctx, `SELECT r.id::text,r.rate::text,r.source_ref,r.utility_rate_plan_id::text,r.plan_chosen_by,r.source_as_of,
 (SELECT count(*) FROM tariff_revision_periods p WHERE p.tariff_revision_id=r.id)
 FROM tariff_revisions r WHERE r.home_id=$1 AND r.fuel='electricity' ORDER BY r.revision`, home)
	if err != nil {
		d.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revisionRow, error) {
		var r revisionRow
		err := row.Scan(&r.id, &r.rate, &r.sourceRef, &r.planID, &r.chosenBy, &r.sourceAsOf, &r.periods)
		return r, err
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

func strp(s string) *string { return &s }

const twoTiersPerDay = `[{"through_kwh":10.0,"per":"day","rate":"0.31000"},{"through_kwh":null,"per":"day","rate":"0.42000"}]`

var allDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
var allMonths = []int16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

func TestRatePlanDefaultPickRankAndNewVersion(t *testing.T) {
	d := newPlanDB(t)
	store, sweep := PGRatePlanStore{Pool: d.pool}, RatePlanSweep{Pool: d.pool}
	d.utilityZIP("92101", "16609", "San Diego Gas & Electric Co", "0.35")
	checked := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	coastal := d.plan(testPlan{eiaid: "16609", key: "urdb:dr-coastal", name: "DR - Coastal", standard: true, tiers: `[{"through_kwh":null,"per":"month","rate":"0.39"}]`, checked: checked})
	official := d.plan(testPlan{eiaid: "16609", key: "official:sdge:tou-dr1", name: "TOU-DR1", standard: true, tiers: twoTiersPerDay, fixed: "0.4", per: "day", checked: checked,
		periods: [][]any{{"on-peak", "0.2", allDays, "16:00", "21:00", allMonths}, {"super off-peak", "-0.1", allDays, "00:00", "06:00", allMonths}}})
	replaced := d.plan(testPlan{eiaid: "16609", key: "urdb:tou-dr-1-coastal", name: "TOU-DR-1 Coastal", standard: false, checked: checked})
	d.exec(`UPDATE utility_rate_plans SET replaced_by_plan_key='official:sdge:tou-dr1' WHERE id=$1`, replaced)
	home := d.home("92101")

	now := time.Now().UTC().Add(-10 * time.Minute)
	if written, err := sweep.RunOnce(d.ctx, now, nil); err != nil || written != 1 {
		t.Fatalf("first sweep wrote %d: %v", written, err)
	}
	revs := d.revisions(home)
	if len(revs) != 1 || *revs[0].planID != official || *revs[0].chosenBy != "default" || revs[0].periods != 2 || !revs[0].sourceAsOf.Equal(checked) {
		t.Fatalf("default revision %+v", revs)
	}
	// res_rate 0.35 sits in tier 1 (0.31 <= 0.35 < 0.42).
	if revs[0].rate != "0.31000000" || revs[0].sourceRef != "https://tariff.test/official:sdge:tou-dr1 sha256:"+strings.Repeat("c", 63)+"1 plan:official:sdge:tou-dr1@v1 base:oedi_res_rate:0.35:tier1" {
		t.Fatalf("rate %s source_ref %q", revs[0].rate, revs[0].sourceRef)
	}
	var utilityRows int
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM home_electric_utility WHERE home_id=$1 AND eiaid='16609' AND chosen_by='zip_single'`, home).Scan(&utilityRows); err != nil || utilityRows != 1 {
		t.Fatalf("zip_single rows %d: %v", utilityRows, err)
	}
	if written, err := sweep.RunOnce(d.ctx, now, nil); err != nil || written != 0 {
		t.Fatalf("rerun wrote %d: %v", written, err)
	}

	view, err := store.RatePlans(d.ctx, d.owner, home, now)
	if err != nil {
		t.Fatal(err)
	}
	if view.Utility == nil || view.Utility.EIAID != "16609" || view.Utility.ChosenBy != "zip_single" || len(view.UtilityCandidates) != 1 ||
		len(view.Plans) != 2 || view.Plans[0].ID != official || view.Plans[1].ID != coastal || view.CurrentPlanID == nil || *view.CurrentPlanID != official {
		t.Fatalf("view %+v", view)
	}
	if p := view.Plans[0]; !p.IsTOU || len(p.Periods) != 2 || p.Periods[1].Adder != "-0.1" || p.Periods[0].StartLocal != "16:00" || p.Source.Kind != "official_tariff" || len(p.Source.Notes) != 1 {
		t.Fatalf("official plan %+v", p)
	}
	if view.Plans[1].IsTOU || view.Plans[1].Periods == nil {
		t.Fatalf("flat plan %+v", view.Plans[1])
	}

	// A new official version gives a new default revision.
	d.exec(`UPDATE utility_rate_plans SET superseded_at=now() WHERE id=$1`, official)
	official2 := d.plan(testPlan{eiaid: "16609", key: "official:sdge:tou-dr1", name: "TOU-DR1", standard: true, tiers: twoTiersPerDay, checked: checked, version: 2,
		periods: [][]any{{"on-peak", "0.25", allDays, "16:00", "21:00", allMonths}}})
	if written, err := sweep.RunOnce(d.ctx, now.Add(time.Minute), nil); err != nil || written != 1 {
		t.Fatalf("new version sweep wrote %d: %v", written, err)
	}
	revs = d.revisions(home)
	if len(revs) != 2 || *revs[1].planID != official2 || *revs[1].chosenBy != "default" || revs[1].periods != 1 {
		t.Fatalf("new version revision %+v", revs)
	}

	// A user pick is rank 0, and the sweep then leaves it alone.
	key, hash := uuid.NewString(), bytes.Repeat([]byte{7}, 32)
	picked, err := store.ChoosePlan(d.ctx, d.owner, home, coastal, key, hash, now.Add(2*time.Minute))
	if err != nil || picked.CurrentPlanID == nil || *picked.CurrentPlanID != coastal {
		t.Fatalf("pick %+v %v", picked, err)
	}
	listed, err := (PGStore{Pool: d.pool}).List(d.ctx, d.owner, home)
	if err != nil || len(listed) != 1 || listed[0].Rate != "0.39000000" || !strings.Contains(listed[0].SourceRef, "plan:urdb:dr-coastal@v1 base:single_tier") {
		t.Fatalf("listed after pick %+v %v", listed, err)
	}
	if replay, err := store.ChoosePlan(d.ctx, d.owner, home, coastal, key, hash, now.Add(3*time.Minute)); err != nil || *replay.CurrentPlanID != coastal {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if _, err = store.ChoosePlan(d.ctx, d.owner, home, official2, key, bytes.Repeat([]byte{8}, 32), now); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse: %v", err)
	}
	if len(d.revisions(home)) != 3 {
		t.Fatal("replay wrote a revision")
	}
	if written, err := sweep.RunOnce(d.ctx, now.Add(4*time.Minute), nil); err != nil || written != 0 {
		t.Fatalf("sweep over a user pick wrote %d: %v", written, err)
	}
	if _, err = store.ChoosePlan(d.ctx, d.owner, home, replaced, uuid.NewString(), hash, now); !errors.Is(err, ErrPlanNotOffered) {
		t.Fatalf("replaced plan pick: %v", err)
	}
	if _, err = store.ChooseUtility(d.ctx, d.owner, home, "999999", uuid.NewString(), hash, now); !errors.Is(err, ErrUnknownUtility) {
		t.Fatalf("unknown utility: %v", err)
	}
	// A later manual tariff replaces the pick, as rank 0 latest.
	fallback := false
	manual := Create{Fuel: "electricity", Rate: "0.2", RateUnit: "kWh", Currency: "USD", RateKind: "user_tariff", SourceRef: "bill", SourceAsOf: now.Format(time.RFC3339), IsFallback: &fallback, EffectiveFrom: time.Now().UTC().Format(time.RFC3339)}
	if _, err = (PGStore{Pool: d.pool}).Create(d.ctx, d.owner, home, manual, uuid.NewString(), hash); err != nil {
		t.Fatal(err)
	}
	if listed, err = (PGStore{Pool: d.pool}).List(d.ctx, d.owner, home); err != nil || listed[0].Rate != "0.20000000" {
		t.Fatalf("manual tariff after pick %+v %v", listed, err)
	}
}

func TestRatePlanSourceAsOfStaysFreshOverFourMonths(t *testing.T) {
	d := newPlanDB(t)
	sweep := RatePlanSweep{Pool: d.pool}
	d.utilityZIP("94612", "14328", "Pacific Gas & Electric Co", "0.30")
	start := time.Now().UTC().Truncate(time.Hour)
	plan := d.plan(testPlan{eiaid: "14328", key: "urdb:e-1", name: "E-1", standard: true, tiers: twoTiersPerDay, checked: start})
	home := d.home("94612")
	for day := 0; day <= 120; day++ {
		at := start.Add(time.Duration(day) * 24 * time.Hour)
		if day%30 == 0 {
			d.exec(`UPDATE utility_rate_plans SET checked_at=$2 WHERE id=$1`, plan, at)
		}
		if _, err := sweep.RunOnce(d.ctx, at.Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		revs := d.revisions(home)
		last := revs[len(revs)-1]
		if !last.sourceAsOf.After(at.Add(-ElectricityMaxAge)) {
			t.Fatalf("day %d: source_as_of %s is outside 90 days", day, last.sourceAsOf)
		}
		if day == 120 && (len(revs) != 5 || *last.chosenBy != "default") {
			t.Fatalf("four months gave %d revisions: %+v", len(revs), revs)
		}
	}
}

func TestRatePlanDefaultNeedsOneStandardPrice(t *testing.T) {
	d := newPlanDB(t)
	sweep := RatePlanSweep{Pool: d.pool}
	checked := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	d.utilityZIP("78701", "1015", "Austin Energy", "0.12")
	d.plan(testPlan{eiaid: "1015", key: "official:austin_energy:standard:inside", name: "Standard", variant: strp("Inside City Limits"), standard: true, tiers: `[{"through_kwh":null,"per":"month","rate":"0.10"}]`, checked: checked})
	d.plan(testPlan{eiaid: "1015", key: "official:austin_energy:standard:outside", name: "Standard", variant: strp("Outside City Limits"), standard: true, tiers: `[{"through_kwh":null,"per":"month","rate":"0.11"}]`, checked: checked})
	austin := d.home("78701")
	d.utilityZIP("94612", "14328", "Pacific Gas & Electric Co", "0.30")
	d.plan(testPlan{eiaid: "14328", key: "official:pge:e-tou-c:x", name: "E-TOU-C", variant: strp("Baseline Territory X"), standard: true,
		tiers: `[{"through_kwh":9.0,"per":"day","rate":"0.31000"},{"through_kwh":null,"per":"day","rate":"0.42000"}]`, checked: checked})
	territoryP := d.plan(testPlan{eiaid: "14328", key: "official:pge:e-tou-c:p", name: "E-TOU-C", variant: strp("Baseline Territory P"), standard: true, tiers: twoTiersPerDay, checked: checked})
	pge := d.home("94612")
	d.utilityZIP("10001", "4226", "Consolidated Edison", "0.25")
	d.utilityZIP("10001", "13573", "National Grid", "0.20")
	several := d.home("10001")
	if _, err := sweep.RunOnce(d.ctx, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if revs := d.revisions(austin); len(revs) != 0 {
		t.Fatalf("Austin variants with different prices got a default: %+v", revs)
	}
	if revs := d.revisions(pge); len(revs) != 1 || *revs[0].planID != territoryP {
		t.Fatalf("variants differing only in allowances: %+v", revs)
	}
	if revs := d.revisions(several); len(revs) != 0 {
		t.Fatalf("several utilities got a default: %+v", revs)
	}
	store := PGRatePlanStore{Pool: d.pool}
	view, err := store.RatePlans(d.ctx, d.owner, several, time.Now().UTC())
	if err != nil || view.Utility != nil || len(view.UtilityCandidates) != 2 || len(view.Plans) != 0 {
		t.Fatalf("several-candidate view %+v %v", view, err)
	}
	d.plan(testPlan{eiaid: "4226", key: "urdb:sc-1", name: "SC-1", standard: true, tiers: `[{"through_kwh":null,"per":"month","rate":"0.30"}]`, checked: checked})
	view, err = store.ChooseUtility(d.ctx, d.owner, several, "4226", uuid.NewString(), bytes.Repeat([]byte{1}, 32), time.Now().UTC())
	if err != nil || view.Utility == nil || view.Utility.ChosenBy != "user" || len(view.Plans) != 1 || view.CurrentPlanID == nil {
		t.Fatalf("utility pick %+v %v", view, err)
	}
	// A ZIP change makes the old choice stale; the new ZIP's single utility is used.
	if _, err = d.pool.Exec(d.ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','94612','postal_code','user_entered')`, several); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-time.Minute)
	if written, err := (RatePlanSweep{Pool: d.pool}).RunOnce(d.ctx, time.Now().UTC(), &since); err != nil || written != 1 {
		t.Fatalf("moved-home pass wrote %d: %v", written, err)
	}
	if revs := d.revisions(several); *revs[len(revs)-1].planID != territoryP {
		t.Fatalf("moved home revisions %+v", revs)
	}
}

func TestRatePlanMeasuredUsageChoosesTier(t *testing.T) {
	d := newPlanDB(t)
	d.utilityZIP("94612", "14328", "Pacific Gas & Electric Co", "0.30")
	checked := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	d.plan(testPlan{eiaid: "14328", key: "urdb:e-1", name: "E-1", standard: true, tiers: twoTiersPerDay, checked: checked})
	home := d.home("94612")
	hub, sensor, appliance, attachment := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	d.exec(`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_rate_plan',$2,'test',$3,1)`, hub, make([]byte, 32), home)
	d.exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,1,'meter','utility_meter')`, sensor, home, hub)
	d.exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Home','refrigerator','{}')`, appliance, home)
	d.exec(`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home)
	d.exec(`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,now()-interval '40 days')`, attachment, home, sensor, appliance)
	now := time.Now().UTC().Truncate(time.Hour)
	// 32 days at 0.5 kWh per hour: 360 kWh in the last 30 days, over the 300 kWh tier-1 allowance (10/day x 30).
	d.exec(`INSERT INTO energy_hourly(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs)
 SELECT $1,$2,1,$3,$4,b,500,3600,1,'complete','exact','[]' FROM generate_series($5::timestamptz-interval '32 days',$5::timestamptz-interval '1 hour',interval '1 hour') b`, home, hub, sensor, attachment, now)
	if _, err := (RatePlanSweep{Pool: d.pool}).RunOnce(d.ctx, now, nil); err != nil {
		t.Fatal(err)
	}
	revs := d.revisions(home)
	if len(revs) != 1 || revs[0].rate != "0.42000000" || !strings.HasSuffix(revs[0].sourceRef, " base:measured_30d_kwh:360.0:tier2") {
		t.Fatalf("measured tier %+v", revs)
	}
}
