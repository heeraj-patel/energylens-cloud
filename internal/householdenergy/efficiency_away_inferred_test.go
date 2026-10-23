package householdenergy

import (
	"context"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// occupancyFixture meters a 0.4 W standby device and a load whose draw in W
// comes from occupied(local time), in 15-minute intervals from start, behind a
// whole-home utility meter.
func occupancyFixture(t *testing.T, start time.Time, days int, loc *time.Location, occupied func(time.Time) string) componentInput {
	return occupancyFixtureMeter(t, start, days, loc, occupied, true)
}

func occupancyFixtureMeter(t *testing.T, start time.Time, days int, loc *time.Location, occupied func(time.Time) string, meter bool) componentInput {
	t.Helper()
	fuel := "electricity"
	end := start.AddDate(0, 0, days)
	items := []interval{}
	rootKind, rootID := "utility_meter", "meter"
	for from := start; from.Before(end); from = from.Add(15 * time.Minute) {
		to := from.Add(15 * time.Minute)
		tv := new(big.Rat).Quo(mustDecimal(occupied(from.In(loc))), big.NewRat(4, 1))
		dyson := interval{Start: from, End: to, Wh: mustDecimal("0.1"), Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug", SensorID: "s-dyson", TopologyComplete: true, TopologyTerminal: true}
		load := interval{Start: from, End: to, Wh: tv, Fuel: &fuel, ApplianceID: "tv", SensorKind: "plug", SensorID: "s-tv", TopologyComplete: true, TopologyTerminal: true}
		if meter {
			dyson.TopologyRootKind, dyson.TopologyRootID = &rootKind, &rootID
			load.TopologyRootKind, load.TopologyRootID = &rootKind, &rootID
			items = append(items, interval{Start: from, End: to, Wh: new(big.Rat).Add(tv, mustDecimal("0.1")), Fuel: &fuel, SensorKind: "utility_meter", SensorID: "meter"})
		}
		items = append(items, dyson, load)
	}
	now := end.Add(time.Hour)
	segment := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: now.Add(-time.Hour)}
	schedule := selectTariffSchedule(now, []interval{{Start: start, End: end, Wh: big.NewRat(1, 1), Fuel: &fuel}}, []marginalTariff{segment})
	if schedule == nil {
		t.Fatal("tariff not selected")
	}
	buckets := makeBuckets(period{Start: start, End: end}, "hour", items)
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	periodRows := sumDeviceAllocations(wh, nil)
	priceAttributedDevices(buckets, rows, periodRows, items, *schedule)
	specs := map[string]splitSpec{"dyson": {StandbyWatts: 0.5}}
	applyMeasuredSplit(buckets, rows, periodRows, items, items, specs, schedule, nil)
	cost := money{Amount: "1.000000", Currency: "USD"}
	return componentInput{Loc: loc, Buckets: buckets, Items: items, Priced: items, Rows: rows, DeviceRows: periodRows, Specs: specs, Tariff: schedule, Metered: map[string]bool{"dyson": true, "tv": true}, Cost: cost}
}

func inWindow(t time.Time, day time.Weekday, from, to int) bool {
	return t.Weekday() == day && t.Hour() >= from && t.Hour() < to
}

// awakeWeek: home 06:00-23:00 at 100 W; Wednesday 09:00-17:00 away, Thursday
// 09:00-17:00 quiet at 2 W, Friday one away hour 12:00-13:00.
func awakeWeek(l time.Time) string {
	switch {
	case l.Hour() < 6 || l.Hour() >= 23:
		return "0"
	case inWindow(l, time.Wednesday, 9, 17), inWindow(l, time.Friday, 12, 13):
		return "0"
	case inWindow(l, time.Thursday, 9, 17):
		return "2"
	}
	return "100"
}

func TestInferredAwayFindsAwayDayNotQuietDay(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	spans := inferAwaySpans(in)
	want := [2]time.Time{time.Date(2026, 10, 14, 9, 0, 0, 0, loc), time.Date(2026, 10, 14, 17, 0, 0, 0, loc)}
	if len(spans) != 1 || !spans[0][0].Equal(want[0]) || !spans[0][1].Equal(want[1]) {
		t.Fatalf("inferred spans = %v, want %v", spans, want)
	}
	res := awayComponent(in)
	got := res.Devices["dyson"]
	// 32 standby intervals in 8 away hours, 0.1 Wh each at 0.10 USD/kWh.
	if wantCost := new(big.Rat).Mul(big.NewRat(32, 1), mustDecimal("0.00001")); got.Amount == nil || got.Amount.Cmp(wantCost) != 0 || got.Basis != "estimated" || res.Source != "baseline_inferred" {
		t.Fatalf("inferred away = %+v %+v", res, got)
	}
	eff := assembleMeasuredEfficiency(in)
	if eff.AwaySource != "baseline_inferred" || eff.AwayBasis != "estimated" || eff.AwayOpportunity == nil || eff.AwayOpportunity.Amount != "0.000320" {
		t.Fatalf("home away = %+v", eff)
	}
}

func TestInferredAwayRunMayEndAtNightButExcludesIt(t *testing.T) {
	loc := nyLoc(t)
	// Saturday leaves at 21:00 and returns Sunday 08:00: away is 21:00-23:00 and 06:00-08:00.
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, func(l time.Time) string {
		if inWindow(l, time.Saturday, 21, 24) || inWindow(l, time.Sunday, 0, 8) {
			return "0"
		}
		return awakeWeek(l)
	})
	spans := inferAwaySpans(in)
	if h := spanHours(spans); len(spans) != 3 || h != 12 {
		t.Fatalf("spans = %v (%v h)", spans, h)
	}
	for _, s := range spans {
		if len(nightSpans(s[0], s[1], loc)) != 0 {
			t.Fatalf("inferred %v overlaps night", s)
		}
	}
	night := nightComponent(in).Devices["dyson"].Amount
	away := awayComponent(in).Devices["dyson"].Amount
	if total := new(big.Rat).Mul(big.NewRat(7*96, 1), mustDecimal("0.00001")); new(big.Rat).Add(night, away).Cmp(total) > 0 {
		t.Fatalf("night %v + away %v exceeds total", night, away)
	}
}

func TestInferredAwayOrder(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	user := awaySchedule{{Days: awayAllDays, StartLocal: "09:00", EndLocal: "10:00"}}
	if res := awayComponentFor(in, user); res.Source != "user_schedule" || res.Devices["dyson"].Basis != "measured" {
		t.Fatalf("user schedule did not win: %+v", res)
	}
	flat := nightFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7*96, splitSpec{StandbyWatts: 0.5}, loc)
	if spans := inferAwaySpans(flat); spans != nil {
		t.Fatalf("flat draw inferred %v", spans)
	}
	if res := awayComponent(flat); res.Source != "default_schedule" {
		t.Fatalf("flat draw source = %s, want default_schedule", res.Source)
	}
}

func TestInferredAwayOnDSTFallBackDay(t *testing.T) {
	loc := nyLoc(t)
	// Sunday 2026-11-01 has 25 hours; away 10:00-14:00 local.
	in := occupancyFixture(t, time.Date(2026, 10, 26, 0, 0, 0, 0, loc), 7, loc, func(l time.Time) string {
		if inWindow(l, time.Sunday, 10, 14) {
			return "0"
		}
		return awakeWeek(l)
	})
	day := civilDay(time.Date(2026, 11, 1, 12, 0, 0, 0, loc), loc)
	var length time.Duration
	for _, d := range homeDraws(in.Items, bucketWindows(in.Buckets), loc) {
		if d.Day == day {
			length += d.End.Sub(d.Start)
		}
	}
	if length != 25*time.Hour {
		t.Fatalf("fall-back day draws cover %v, want 25h", length)
	}
	var sunday [][2]time.Time
	for _, s := range inferAwaySpans(in) {
		if civilDay(s[0], loc) == day {
			sunday = append(sunday, s)
		}
	}
	if len(sunday) != 1 || !sunday[0][0].Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, loc)) || spanHours(sunday) != 4 {
		t.Fatalf("DST day spans = %v", sunday)
	}
}

// quietWeek is awakeWeek with Thursday 09:00-17:00 at 0.05 W on the TV plug,
// a home draw of 0.45 W: within 1.2 x 0.4 W but above 0.4 W.
func quietWeek(l time.Time) string {
	if inWindow(l, time.Thursday, 9, 17) {
		return "0.05"
	}
	return awakeWeek(l)
}

func TestInferredAwayThresholdWithoutStandbyFactsIsP10Ratio(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, quietWeek)
	if h := spanHours(inferAwaySpans(in)); h != 16 {
		t.Fatalf("1.2 x p10 away = %v h, want 16 (Wed + Thu)", h)
	}
}

func TestInferredAwayThresholdAddsEveryDeviceStandby(t *testing.T) {
	loc := nyLoc(t)
	// Fridge has no plug but its standby is in the whole-home draw: 0.4 + 2.5 W admits Thursday at 2.4 W.
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	in.Standby = map[string]*big.Rat{"fridge": big.NewRat(5, 2)}
	if h := spanHours(inferAwaySpans(in)); h != 16 {
		t.Fatalf("p10 + standby away = %v h, want 16", h)
	}
	// Facts present: the threshold is p10 + standby, not 1.2 x p10, so 0.45 W is not low at p10 + 0.04 W.
	in = occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, quietWeek)
	in.Standby = map[string]*big.Rat{"tv": big.NewRat(1, 25)}
	if h := spanHours(inferAwaySpans(in)); h != 8 {
		t.Fatalf("p10 + 0.04 W away = %v h, want 8 (Wed only)", h)
	}
}

func TestInferredAwayNeedsWholeHomeMeter(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixtureMeter(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek, false)
	if spans := inferAwaySpans(in); spans != nil {
		t.Fatalf("plug-only home inferred %v", spans)
	}
	if res := awayComponent(in); res.Source != "default_schedule" {
		t.Fatalf("plug-only source = %s, want default_schedule", res.Source)
	}
}

func TestInferredAwayBaselineReadsTheWeekBeforeThePeriod(t *testing.T) {
	loc := nyLoc(t)
	// Period is 2026-10-01 only, away all day at 0.4 W; the week before is home 06:00-23:00 at 100 W.
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	week := occupancyFixture(t, day.AddDate(0, 0, -7), 8, loc, func(l time.Time) string {
		if l.Month() == time.October && l.Day() == 1 || l.Hour() < 6 || l.Hour() >= 23 {
			return "0"
		}
		return "100"
	})
	var lookback, items []interval
	for _, v := range week.Items {
		if v.Start.Before(day) {
			lookback = append(lookback, v)
		} else {
			items = append(items, v)
		}
	}
	in := componentInput{Loc: loc, Items: items, Buckets: makeBuckets(period{Start: day, End: day.AddDate(0, 0, 1)}, "hour", items)}
	if spans := inferAwaySpans(in); spans != nil {
		t.Fatalf("flat day without lookback inferred %v", spans)
	}
	in.Lookback = lookback
	want := [2]time.Time{day.Add(6 * time.Hour), day.Add(23 * time.Hour)}
	if spans := inferAwaySpans(in); len(spans) != 1 || spans[0] != want {
		t.Fatalf("with lookback spans = %v, want %v", spans, want)
	}
}

// meterInput is a whole-home utility meter with the given 15-minute draws in W.
func meterInput(t *testing.T, loc *time.Location, start time.Time, draws map[time.Time]string) componentInput {
	t.Helper()
	fuel := "electricity"
	var items []interval
	for from, w := range draws {
		items = append(items, interval{Start: from, End: from.Add(15 * time.Minute), Wh: new(big.Rat).Quo(mustDecimal(w), big.NewRat(4, 1)), Fuel: &fuel, SensorKind: "utility_meter", SensorID: "meter"})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
	return componentInput{Loc: loc, Buckets: makeBuckets(period{Start: start, End: start.AddDate(0, 0, 1)}, "hour", items), Items: items}
}

func TestInferredAwayWholeHomeMeterAddsEveryDeviceStandby(t *testing.T) {
	loc := nyLoc(t)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, loc)
	draws := map[time.Time]string{}
	for m := 7 * 60; m < 22*60; m += 15 {
		w := "300"
		if m >= 10*60 && m < 14*60 {
			w = "3"
		} else if m < 8*60+30 {
			w = "1"
		}
		draws[day.Add(time.Duration(m)*time.Minute)] = w
	}
	in := meterInput(t, loc, day, draws)
	if spans := inferAwaySpans(in); spanHours(spans) != 0 {
		t.Fatalf("1.2 x 1 W inferred %v", spans)
	}
	in.Standby = map[string]*big.Rat{"fridge": big.NewRat(5, 2)}
	want := [2]time.Time{day.Add(10 * time.Hour), day.Add(14 * time.Hour)}
	if spans := inferAwaySpans(in); len(spans) != 1 || spans[0] != want {
		t.Fatalf("1 + 2.5 W spans = %v, want %v", spans, want)
	}
}

func TestInferredAwayGapsDoNotBreakARun(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, awakeWeek)
	gapFrom, gapTo := time.Date(2026, 10, 14, 11, 0, 0, 0, loc), time.Date(2026, 10, 14, 15, 0, 0, 0, loc)
	var kept []interval
	for _, v := range in.Items {
		if v.Start.Before(gapFrom) || !v.Start.Before(gapTo) {
			kept = append(kept, v)
		}
	}
	in.Items = kept
	want := [2]time.Time{time.Date(2026, 10, 14, 9, 0, 0, 0, loc), time.Date(2026, 10, 14, 17, 0, 0, 0, loc)}
	if spans := inferAwaySpans(in); len(spans) != 1 || spans[0] != want {
		t.Fatalf("gapped spans = %v, want %v", spans, want)
	}
}

func TestInferredAwayNonLowIntervalSplitsTheRun(t *testing.T) {
	loc := nyLoc(t)
	in := occupancyFixture(t, time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 7, loc, func(l time.Time) string {
		if inWindow(l, time.Wednesday, 13, 14) && l.Minute() == 0 {
			return "100"
		}
		return awakeWeek(l)
	})
	spans := inferAwaySpans(in)
	if len(spans) != 2 || !spans[0][1].Equal(time.Date(2026, 10, 14, 13, 0, 0, 0, loc)) || !spans[1][0].Equal(time.Date(2026, 10, 14, 13, 15, 0, 0, loc)) {
		t.Fatalf("split spans = %v", spans)
	}
}

func TestInferredAwayFromSparseIntervals(t *testing.T) {
	loc := nyLoc(t)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, loc)
	in := meterInput(t, loc, day, map[time.Time]string{
		day.Add(8 * time.Hour):                 "100",
		day.Add(10 * time.Hour):                "1",
		day.Add(12*time.Hour + 30*time.Minute): "1",
	})
	want := [2]time.Time{day.Add(10 * time.Hour), day.Add(12*time.Hour + 45*time.Minute)}
	if spans := inferAwaySpans(in); len(spans) != 1 || spans[0] != want {
		t.Fatalf("sparse spans = %v, want %v", spans, want)
	}
	in = meterInput(t, loc, day, map[time.Time]string{day.Add(8 * time.Hour): "100", day.Add(10 * time.Hour): "1", day.Add(11 * time.Hour): "1"})
	if spans := inferAwaySpans(in); spans != nil {
		t.Fatalf("1h15m span inferred %v", spans)
	}
}

func TestStandbyWattsKeepsExactWattFactsOnce(t *testing.T) {
	ok := standbyFact{ApplianceID: "tv", Model: "TV1", MatchQuality: "confirmed_exact", DocumentedScope: "tv1", MatchedModel: "TV1", Unit: "W", Value: "0.5"}
	nonW, family, scope, dup := ok, ok, ok, ok
	nonW.ApplianceID, nonW.Unit = "a", "kWh/hour"
	family.ApplianceID, family.MatchQuality = "b", "documented_variant_family"
	scope.ApplianceID, scope.DocumentedScope = "c", "TV*"
	dup.ApplianceID = "d"
	got := standbyWatts([]standbyFact{ok, nonW, family, scope, dup, dup})
	if len(got) != 1 || got["tv"] == nil || got["tv"].Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("standby = %v", got)
	}
}

func TestPostgresStandbyWattsFromActiveExactFacts(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "standby_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, home, tv, speaker, old := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO users(id) VALUES($1)`, owner)
	exec(`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, home, owner)
	appliance := func(id, model string, archived bool) {
		t.Helper()
		exec(`INSERT INTO appliances(id,home_id,name,category,identity,archived_at) VALUES($1,$2,'Device','television',jsonb_build_object('brand','acme','full_model',$3::text,'variant','us'),CASE WHEN $4 THEN now() END)`, id, home, model, archived)
	}
	appliance(tv, "tv1", false)
	appliance(speaker, "SP1", false)
	appliance(old, "OLD1", true)
	hash := strings.Repeat("a", 64)
	exec(`INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'test',3)`, hash)
	fact := func(model, scope, value string) {
		t.Helper()
		exec(`INSERT INTO reviewed_usage_facts(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
  VALUES($1,$2,'television','Acme',$3,'electricity','US','confirmed_exact',$4,$3,'standby_power',$5::numeric,'W','Official test',NULL,'Standby','{"status":"documented","components":["standby_electricity"]}','oem_pdf','https://example.test/manual',$2,0,$2,'reviewer',now(),'test.json','{}')`, uuid.NewString(), hash, model, scope, value)
	}
	fact("TV1", "TV1", "0.5")
	fact("SP1", "SP*", "1.5")
	fact("OLD1", "OLD1", "3")
	got, err := loadStandbyWatts(ctx, pool, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[tv] == nil || got[tv].Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("standby = %v, want only %s at 0.5 W", got, tv)
	}
}
