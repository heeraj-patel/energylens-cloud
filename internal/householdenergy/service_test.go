package householdenergy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	tariffpkg "energylens.local/cloud/internal/tariff"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if got := len(makeBuckets(p, "hour", nil)); got != 23 {
		t.Fatalf("spring hourly buckets = %d", got)
	}
	p = periodFor(time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC), loc, "hour")
	if got := p.End.Sub(p.Start); got != 25*time.Hour {
		t.Fatalf("fall day length = %v", got)
	}
	buckets := makeBuckets(p, "hour", nil)
	if len(buckets) != 25 {
		t.Fatalf("fall hourly buckets = %d", len(buckets))
	}
	for i := 1; i < len(buckets); i++ {
		if !buckets[i-1].End.Equal(buckets[i].Start) || buckets[i].End.Sub(buckets[i].Start) != time.Hour {
			t.Fatalf("DST buckets are discontinuous at %d", i)
		}
	}
}

func TestTrailing30DaysUsesHomeLocalCalendarBounds(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	for _, test := range []struct {
		name      string
		now       time.Time
		duration  time.Duration
		dstBucket time.Duration
	}{
		{name: "spring forward", now: time.Date(2026, 3, 10, 18, 0, 0, 0, time.UTC), duration: 719 * time.Hour, dstBucket: 23 * time.Hour},
		{name: "fall back", now: time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC), duration: 721 * time.Hour, dstBucket: 25 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := trailing30DaysPeriod(test.now, loc)
			local := test.now.In(loc)
			today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
			if !p.Start.Equal(today.AddDate(0, 0, -29)) || !p.End.Equal(today.AddDate(0, 0, 1)) || p.End.Sub(p.Start) != test.duration {
				t.Fatalf("unexpected home-local window: %+v duration=%v", p, p.End.Sub(p.Start))
			}
			buckets := makeBuckets(p, "day", nil)
			if len(buckets) != 30 {
				t.Fatalf("trailing window has %d daily buckets", len(buckets))
			}
			foundDSTDay := false
			for _, bucket := range buckets {
				if bucket.End.Sub(bucket.Start) == test.dstBucket {
					foundDSTDay = true
				}
			}
			if !foundDSTDay {
				t.Fatalf("trailing window lacks expected %v DST day", test.dstBucket)
			}
		})
	}
}

func TestTrailing30DaysCrossesMonthBoundaryAndLeapDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2028, 3, 1, 18, 0, 0, 0, time.UTC)
	p := trailing30DaysPeriod(now, loc)
	if got, want := p.Start.In(loc).Format("2006-01-02"), "2028-02-01"; got != want {
		t.Fatalf("rolling start = %s, want %s", got, want)
	}
	if got, want := p.End.In(loc).Format("2006-01-02"), "2028-03-02"; got != want {
		t.Fatalf("rolling exclusive end = %s, want %s", got, want)
	}
	buckets := makeBuckets(p, "day", nil)
	if len(buckets) != 30 || buckets[28].Start.In(loc).Day() != 29 || buckets[29].Start.In(loc).Day() != 1 {
		t.Fatalf("rolling buckets omit leap day or current day: %+v", buckets)
	}
}

func TestLastFiveOccurrencesAreHomeLocalAcrossDSTAndCalendarBoundaries(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	for _, tc := range []struct {
		name, granularity  string
		now                time.Time
		wantStart, wantEnd string
		wantBuckets        int
		wantDST            time.Duration
	}{
		{name: "days-cross-spring-DST", granularity: "day", now: time.Date(2026, 3, 10, 12, 0, 0, 0, loc), wantStart: "2026-03-06", wantEnd: "2026-03-11", wantBuckets: 5, wantDST: 23 * time.Hour},
		{name: "weeks-cross-month-and-DST", granularity: "week", now: time.Date(2026, 3, 10, 12, 0, 0, 0, loc), wantStart: "2026-02-09", wantEnd: "2026-03-16", wantBuckets: 5, wantDST: 167 * time.Hour},
		{name: "months-cross-year", granularity: "month", now: time.Date(2026, 2, 15, 12, 0, 0, 0, loc), wantStart: "2025-10-01", wantEnd: "2026-03-01", wantBuckets: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := last5OccurrencesPeriod(tc.now, loc, tc.granularity)
			if p.Start.In(loc).Format("2006-01-02") != tc.wantStart || p.End.In(loc).Format("2006-01-02") != tc.wantEnd {
				t.Fatalf("window bounds: %s through %s", p.Start.In(loc).Format("2006-01-02"), p.End.In(loc).Format("2006-01-02"))
			}
			buckets := makeBuckets(p, tc.granularity, nil)
			if len(buckets) != tc.wantBuckets {
				t.Fatalf("got %d occurrences", len(buckets))
			}
			for i := 1; i < len(buckets); i++ {
				if !buckets[i-1].End.Equal(buckets[i].Start) {
					t.Fatalf("bucket gap at %d: %+v %+v", i, buckets[i-1], buckets[i])
				}
			}
			if tc.wantDST != 0 {
				found := false
				for _, b := range buckets {
					if b.End.Sub(b.Start) == tc.wantDST {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing DST bucket duration %v", tc.wantDST)
				}
			}
		})
	}
}

func TestExistingCalendarPeriodsStayUnchangedWithoutWindow(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ granularity, start, end string }{
		{"hour", "2026-10-06", "2026-10-07"},
		{"day", "2026-10-05", "2026-10-12"},
		{"week", "2026-10-01", "2026-11-01"},
		{"month", "2026-01-01", "2027-01-01"},
	} {
		p := periodFor(now, loc, tc.granularity)
		if p.Start.In(loc).Format("2006-01-02") != tc.start || p.End.In(loc).Format("2006-01-02") != tc.end {
			t.Errorf("%s calendar period changed: %s to %s", tc.granularity, p.Start.In(loc), p.End.In(loc))
		}
	}
}

func TestTrendsQueryPreservesLegacyAndAcceptsNamedWindows(t *testing.T) {
	for _, test := range []struct {
		query url.Values
		wantG string
		wantW string
		valid bool
	}{
		{query: url.Values{"granularity": {"day"}}, wantG: "day", valid: true},
		{query: url.Values{"granularity": {"day"}, "window": {"trailing_30_days"}}, wantG: "day", wantW: "trailing_30_days", valid: true},
		{query: url.Values{"granularity": {"day"}, "window": {"month_to_date"}}, wantG: "day", wantW: "month_to_date", valid: true},
		{query: url.Values{"granularity": {"day"}, "window": {"last_5_occurrences"}}, wantG: "day", wantW: "last_5_occurrences", valid: true},
		{query: url.Values{"granularity": {"week"}, "window": {"last_5_occurrences"}}, wantG: "week", wantW: "last_5_occurrences", valid: true},
		{query: url.Values{"granularity": {"month"}, "window": {"last_5_occurrences"}}, wantG: "month", wantW: "last_5_occurrences", valid: true},
		{query: url.Values{"granularity": {"hour"}, "window": {"trailing_30_days"}}},
		{query: url.Values{"granularity": {"hour"}, "window": {"last_5_occurrences"}}},
		{query: url.Values{"granularity": {"week"}, "window": {"trailing_30_days"}}},
		{query: url.Values{"granularity": {"hour"}, "window": {"month_to_date"}}},
		{query: url.Values{"granularity": {"week"}, "window": {"month_to_date"}}},
		{query: url.Values{"granularity": {"day", "week"}}},
		{query: url.Values{"granularity": {"day"}, "other": {"1"}}},
	} {
		g, window, ok := parseTrendsQuery(test.query)
		if ok != test.valid || g != test.wantG || window != test.wantW {
			t.Fatalf("parseTrendsQuery(%v) = %q, %q, %v", test.query, g, window, ok)
		}
	}
}

func TestMonthToDatePeriodUsesHomeLocalCalendar(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		utc, start, end string
	}{
		{"2026-11-01T04:30:00Z", "2026-10-01", "2026-11-01"},
		{"2026-11-01T06:30:00Z", "2026-11-01", "2026-11-02"},
		{"2026-11-10T18:00:00Z", "2026-11-01", "2026-11-11"},
	} {
		now, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		p := monthToDatePeriod(now, loc)
		if got := p.Start.In(loc).Format("2006-01-02"); got != tc.start {
			t.Errorf("%s start = %s, want %s", tc.utc, got, tc.start)
		}
		if got := p.End.In(loc).Format("2006-01-02"); got != tc.end {
			t.Errorf("%s end = %s, want %s", tc.utc, got, tc.end)
		}
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

func TestRealDeviceAttributionOnlyForDirectUnambiguousMeters(t *testing.T) {
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	first, second := uuid.NewString(), uuid.NewString()
	parent := uuid.NewString()
	electricity := "electricity"
	items := []interval{
		{Start: start, End: start.Add(15 * time.Minute), Wh: big.NewRat(1000, 1), Fuel: &electricity, ApplianceID: first, SensorKind: "plug"},
		{Start: start.Add(15 * time.Minute), End: start.Add(30 * time.Minute), Wh: big.NewRat(500, 1), Fuel: &electricity, ApplianceID: second, SensorKind: "strip_outlet"},
		{Start: start.Add(30 * time.Minute), End: start.Add(45 * time.Minute), Wh: big.NewRat(250, 1), Fuel: &electricity, ApplianceID: first, SensorKind: "circuit"},
		{Start: start.Add(45 * time.Minute), End: start.Add(time.Hour), Wh: big.NewRat(250, 1), Fuel: &electricity, ApplianceID: first, SensorKind: "plug", ParentSensorID: &parent},
	}
	buckets := makeBuckets(period{start, start.Add(time.Hour)}, "hour", items)
	tariff := &marginalTariff{Rate: big.NewRat(20, 100), Currency: "USD"}
	priceBuckets(buckets, *tariff)
	rows, wh := attributeDeviceBuckets(buckets, items, tariff)
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0].ApplianceID != min(first, second) {
		t.Fatalf("unexpected measured rows: %+v", rows)
	}
	if wh[first].Cmp(big.NewRat(1000, 1)) != 0 || wh[second].Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("broad or nested meter was attributed: %+v", wh)
	}
	periodRows := sumDeviceAllocations(wh, tariff)
	var deviceCost big.Rat
	for _, row := range periodRows {
		a := row.Allocation
		if a.UsefulCost != nil || a.WasteCost != nil || a.UsefulEnergy != nil || a.WasteEnergy != nil {
			t.Fatalf("unsupported useful/waste split: %+v", a)
		}
		v, _ := new(big.Rat).SetString(a.Cost.(money).Amount)
		deviceCost.Add(&deviceCost, v)
	}
	if deviceCost.Cmp(big.NewRat(30, 100)) != 0 || buckets[0].Allocation.Cost.(money).Amount != "0.300000" {
		t.Fatalf("device allocation did not stay within home cost: rows=%+v bucket=%+v", periodRows, buckets[0])
	}
	items[1].Start = start.Add(10 * time.Minute)
	ambiguousBuckets := makeBuckets(period{start, start.Add(time.Hour)}, "hour", items)
	ambiguousRows, _ := attributeDeviceBuckets(ambiguousBuckets, items, tariff)
	if len(ambiguousRows[0]) != 0 || ambiguousBuckets[0].Allocation.Energy != nil {
		t.Fatalf("overlapping topology was attributed: %+v", ambiguousRows)
	}
}

func TestIntervalCrossingBucketAndPeriodBoundariesIsProportionallyAllocated(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	periodStart := start.Add(time.Hour)
	periodEnd := periodStart.Add(time.Hour)
	items := []interval{{Start: start.Add(30 * time.Minute), End: start.Add(90 * time.Minute), Wh: big.NewRat(12, 1)}}
	buckets := makeBuckets(period{periodStart, periodEnd}, "hour", items)
	if len(buckets) != 1 || buckets[0].Allocation.Energy == nil || buckets[0].Allocation.Energy.Value != "0.006000000000" {
		t.Fatalf("period-edge interval allocation = %+v", buckets)
	}
	full := makeBuckets(period{start, start.Add(2 * time.Hour)}, "hour", items)
	if len(full) != 2 || full[0].Allocation.Energy.Value != "0.006000000000" || full[1].Allocation.Energy.Value != "0.006000000000" {
		t.Fatalf("bucket-seam interval was not split evenly: %+v", full)
	}
}

func TestClippedIntervalsConserveEnergyAcrossAdjacentWindows(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	original := interval{Start: start.Add(30 * time.Minute), End: start.Add(3 * time.Hour), Wh: big.NewRat(125, 1)}
	var allocated big.Rat
	for i, want := range []int64{25, 50, 50} {
		v := original
		if !clipInterval(&v, period{start.Add(time.Duration(i) * time.Hour), start.Add(time.Duration(i+1) * time.Hour)}) || v.Wh.Cmp(big.NewRat(want, 1)) != 0 {
			t.Fatalf("window %d allocation = %s, want %d Wh", i, v.Wh, want)
		}
		allocated.Add(&allocated, v.Wh)
	}
	if allocated.Cmp(original.Wh) != 0 {
		t.Fatalf("adjacent windows allocated %s of %s Wh", &allocated, original.Wh)
	}
	outside := original
	if clipInterval(&outside, period{start.Add(3 * time.Hour), start.Add(4 * time.Hour)}) {
		t.Fatal("interval outside the query boundary was included")
	}
}

func TestFractionalBucketEnergyAndMoneyConserveSerializedTotals(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	items := []interval{{Start: start, End: start.Add(3 * time.Hour), Wh: big.NewRat(1, 1)}}
	hourly := makeBuckets(period{start, start.Add(3 * time.Hour)}, "hour", items)
	daily := makeBuckets(period{start, start.Add(3 * time.Hour)}, "day", items)
	tariff := marginalTariff{Rate: big.NewRat(1, 1), Currency: "USD"}
	hourlyCost, dailyCost := priceBuckets(hourly, tariff), priceBuckets(daily, tariff)
	if hourlyCost.Amount != "0.001000" || dailyCost.Amount != hourlyCost.Amount {
		t.Fatalf("fractional interval cost varies with granularity: hourly=%+v daily=%+v", hourlyCost, dailyCost)
	}
	var energy, cost big.Rat
	for _, b := range hourly {
		energy.Add(&energy, mustDecimal(b.Allocation.Energy.Value))
		cost.Add(&cost, mustDecimal(b.Allocation.Cost.(money).Amount))
	}
	if energy.Cmp(big.NewRat(1, 1000)) != 0 || cost.Cmp(mustDecimal(hourlyCost.Amount)) != 0 {
		t.Fatalf("serialized buckets do not conserve totals: energy=%s cost=%s", &energy, &cost)
	}
	// Each exact bucket cost rounds upward in isolation. Cumulative rounding
	// must keep the correction distributed without creating a negative bucket.
	tiny := make([]interval, 24)
	for i := range tiny {
		tiny[i] = interval{Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i+1) * time.Hour), Wh: big.NewRat(6, 10000)}
	}
	tinyBuckets := makeBuckets(period{start, start.Add(24 * time.Hour)}, "hour", tiny)
	tinyTotal := priceBuckets(tinyBuckets, tariff)
	cost.SetInt64(0)
	for _, b := range tinyBuckets {
		amount := mustDecimal(b.Allocation.Cost.(money).Amount)
		if amount.Sign() < 0 {
			t.Fatalf("positive interval produced negative bucket cost: %s", amount)
		}
		cost.Add(&cost, amount)
	}
	if cost.Cmp(mustDecimal(tinyTotal.Amount)) != 0 {
		t.Fatalf("tiny bucket costs sum to %s, total is %s", &cost, tinyTotal.Amount)
	}
}

func TestSubCentCostsAreIndependentOfBucketGranularity(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	items := make([]interval, 24)
	for i := range items {
		items[i] = interval{Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i+1) * time.Hour), Wh: big.NewRat(10, 1)}
	}
	tariff := marginalTariff{Rate: big.NewRat(15, 100), Currency: "USD"}
	hourly := makeBuckets(period{start, start.Add(24 * time.Hour)}, "hour", items)
	daily := makeBuckets(period{start, start.Add(24 * time.Hour)}, "day", items)
	hourlyTotal := priceBuckets(hourly, tariff)
	dailyTotal := priceBuckets(daily, tariff)
	if hourlyTotal == nil || dailyTotal == nil || hourlyTotal.Amount != "0.036000" || dailyTotal.Amount != hourlyTotal.Amount {
		t.Fatalf("sub-cent total depends on granularity: hourly=%+v daily=%+v", hourlyTotal, dailyTotal)
	}
	var hourlyBuckets big.Rat
	for _, b := range hourly {
		amount := b.Allocation.Cost.(money).Amount
		v, ok := new(big.Rat).SetString(amount)
		if !ok {
			t.Fatalf("invalid bucket cost %q", amount)
		}
		hourlyBuckets.Add(&hourlyBuckets, v)
	}
	if hourlyBuckets.Cmp(big.NewRat(36, 1000)) != 0 {
		t.Fatalf("hourly sub-cent costs did not reconcile: %s", &hourlyBuckets)
	}
}

func TestMeasuredCostReconcilesAndKeepsClassificationsUnknown(t *testing.T) {
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	buckets := makeBuckets(period{start, start.Add(time.Hour)}, "hour", []interval{
		{Start: start.Add(time.Minute), End: start.Add(2 * time.Minute), Wh: big.NewRat(1500, 1)},
	})
	cost := priceBuckets(buckets, marginalTariff{Rate: big.NewRat(20, 100), Currency: "USD"})
	if cost == nil || cost.Amount != "0.300000" || buckets[0].Allocation.Cost.(money).Amount != cost.Amount || buckets[0].Allocation.UnclassifiedCost.(money).Amount != cost.Amount {
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
	demoSnapshot := demoSnapshotForTest()
	if err = ImportDemoCostSnapshot(ctx, pool, other.ID, home.ID, demoSnapshot); err == nil {
		t.Fatal("snapshot importer accepted a different owner")
	}
	if err = ImportDemoCostSnapshot(ctx, pool, owner.ID, home.ID, demoSnapshot); err != nil {
		t.Fatal(err)
	}
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
	if code, sample := getSample(dash, "owner"); code != 200 || sample["data_mode"] != "sample" || sample["fixture_revision"] != DemoSnapshotRevision || sample["evidence_basis"] != "synthetic" || len(sample["device_allocations"].([]any)) != 18 || sample["totals"].(map[string]any)["energy"] != nil || sample["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != "2.469984" || sample["scope"] != "whole_home_including_unattributed_load" {
		t.Fatalf("missing sample response: %d %+v", code, sample)
	}
	_, sampleResponse := getSample(dash, "owner")
	if sampleResponse["as_of"] != now.Format(time.RFC3339) {
		t.Fatalf("demo response lacks request-time as_of: %v", sampleResponse["as_of"])
	}
	_, weekSeries := getSample("/v1/homes/"+home.ID+"/energy-trends?granularity=day", "owner")
	weekBuckets := weekSeries["buckets"].([]any)
	todayBucket := weekBuckets[1].(map[string]any) // Fixed test clock is Tuesday in the home timezone.
	if weekSeries["data_mode"] != sampleResponse["data_mode"] || weekSeries["as_of"] != sampleResponse["as_of"] ||
		todayBucket["allocation"].(map[string]any)["cost"].(map[string]any)["amount"] != sampleResponse["totals"].(map[string]any)["cost"].(map[string]any)["amount"] {
		t.Fatal("demo day, week-to-date series, and dashboard do not share an accrued current day")
	}
	for _, granularity := range []string{"day", "week", "month"} {
		path := "/v1/homes/" + home.ID + "/energy-trends?granularity=" + granularity + "&window=last_5_occurrences"
		code, series := getSample(path, "owner")
		if code != 200 || series["data_mode"] != "sample" || series["as_of"] != sampleResponse["as_of"] || len(series["buckets"].([]any)) != 5 {
			t.Fatalf("demo %s occurrence response diverged: %d mode=%v as_of=%v", granularity, code, series["data_mode"], series["as_of"])
		}
	}
	firstDevice := sampleResponse["device_allocations"].([]any)[0].(map[string]any)
	if firstDevice["device_id"] != "demo-device-01" || firstDevice["name"] != "Study Desktop & Rig" || firstDevice["appliance_id"] != nil {
		t.Fatalf("unexpected synthetic identity shape: %+v", firstDevice)
	}
	if code, hourly := getSample(trend, "owner"); code != 200 || sampleResponse["snapshot_id"] != hourly["snapshot_id"] || !reflect.DeepEqual(sampleResponse["period"], hourly["period"]) || !reflect.DeepEqual(sampleResponse["totals"], hourly["totals"]) {
		t.Fatalf("dashboard and hourly trend snapshots diverged: dashboard=%+v hourly=%+v", sampleResponse, hourly)
	}
	if code, sample := getSample(trend, "owner"); code != 200 || sample["data_mode"] != "sample" || len(sample["buckets"].([]any)) != 24 {
		t.Fatalf("missing sample trend response: %d %+v", code, sample)
	} else {
		for index, raw := range sample["buckets"].([]any) {
			row := raw.(map[string]any)
			if index <= 13 {
				if len(row["device_allocations"].([]any)) != 18 || row["allocation"].(map[string]any)["energy"] != nil || row["unattributed"] == nil {
					t.Fatalf("sample trend bucket lacks device allocations: %+v", row)
				}
			} else if row["allocation"].(map[string]any)["energy"] != nil || row["allocation"].(map[string]any)["availability"] != "unavailable" {
				t.Fatalf("future trend bucket is not unavailable: %+v", row)
			}
		}
	}
	trailingTrend := "/v1/homes/" + home.ID + "/energy-trends?granularity=day&window=trailing_30_days"
	if code, sample := getSample(trailingTrend, "owner"); code != 200 || sample["granularity"] != "day" || len(sample["buckets"].([]any)) != 30 || sample["period"].(map[string]any)["start_local"] != "2026-09-07T00:00:00-05:00" || sample["period"].(map[string]any)["end_exclusive_local"] != "2026-10-07T00:00:00-05:00" {
		t.Fatalf("trailing 30 sample window mismatch: %d %+v", code, sample)
	} else {
		if sample["efficiency_month_to_date"] != nil {
			t.Fatal("MTD efficiency leaked into trailing window")
		}
		rolling, ok := sample["efficiency_trailing_30_days"].(map[string]any)
		if !ok || !reflect.DeepEqual(rolling["period_cost"], sample["totals"].(map[string]any)["cost"]) ||
			!reflect.DeepEqual(rolling["waste_opportunity"], sample["totals"].(map[string]any)["waste_cost"]) {
			t.Fatalf("rolling efficiency not tied to rolling totals: %+v", sample)
		}
		last := sample["buckets"].([]any)[29].(map[string]any)
		if last["allocation"].(map[string]any)["cost"].(map[string]any)["amount"] != "2.469984" || last["allocation"].(map[string]any)["energy"] != nil {
			t.Fatal("daily bucket did not accrue through current local time")
		}
	}
	mtdTrend := "/v1/homes/" + home.ID + "/energy-trends?granularity=day&window=month_to_date"
	if code, sample := getSample(mtdTrend, "owner"); code != 200 {
		t.Fatalf("demo MTD read failed: %d %+v", code, sample)
	} else {
		efficiency, ok := sample["efficiency_month_to_date"].(map[string]any)
		if !ok || efficiency["evidence_basis"] != "synthetic" ||
			efficiency["waste_timing_basis"] != "unknown_75_25_demo_fallback" ||
			!reflect.DeepEqual(efficiency["month_to_date_cost"], sample["totals"].(map[string]any)["cost"]) ||
			!reflect.DeepEqual(efficiency["waste_opportunity"], sample["totals"].(map[string]any)["waste_cost"]) {
			t.Fatalf("demo MTD projection not tied to same-home totals: %+v", sample)
		}
		widths, ok := efficiency["chart_segment_width_percent"].(map[string]any)
		if !ok || len(widths) != 4 || widths["night"] == nil || widths["away"] == nil || widths["peak_rates"] == nil || widths["habits"] == nil {
			t.Fatalf("demo MTD projection lacks final chart widths: %+v", efficiency)
		}
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
	if code, response := getSample(dash, "owner"); code != 200 || response["data_mode"] != "mixed" || response["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != "2.469984" || len(response["device_allocations"].([]any)) != 18 {
		t.Fatalf("real inventory did not retain seeded values in mixed mode: %d %+v", code, response)
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
	if live["data_mode"] != "mixed" || live["as_of"] != end.UTC().Format(time.RFC3339) || len(live["device_allocations"].([]any)) != 19 {
		t.Fatalf("mixed dashboard lost seeded rows or reported wrong home as_of: mode=%v as_of=%v", live["data_mode"], live["as_of"])
	}
	if code, mixedMTD := get(mtdTrend, "owner"); code != 200 || mixedMTD["efficiency_month_to_date"] != nil {
		t.Fatalf("demo opportunity attached to measured home total: %d %+v", code, mixedMTD)
	}
	real := live["device_allocations"].([]any)[18].(map[string]any)
	if real["appliance_id"] != appliance || real["allocation"].(map[string]any)["energy"].(map[string]any)["value"] != "1.500000000000" {
		t.Fatalf("live appliance was not attributed: %+v", real)
	}
	for _, granularity := range []string{"day", "week", "month"} {
		path := "/v1/homes/" + home.ID + "/energy-trends?granularity=" + granularity + "&window=last_5_occurrences"
		code, series := get(path, "owner")
		if code != 200 || series["data_mode"] != "mixed" || series["as_of"] != live["as_of"] || len(series["buckets"].([]any)) != 5 || series["unattributed"] != nil {
			t.Fatalf("live %s occurrence response diverged: %d mode=%v as_of=%v", granularity, code, series["data_mode"], series["as_of"])
		}
	}
	code, trailingLive := get(trailingTrend, "owner")
	if code != 200 || trailingLive["granularity"] != "day" || len(trailingLive["buckets"].([]any)) != 30 {
		t.Fatalf("live trailing 30 trend: %d %+v", code, trailingLive)
	}
	lastBucket := trailingLive["buckets"].([]any)[29].(map[string]any)
	if lastBucket["allocation"].(map[string]any)["energy"].(map[string]any)["value"] != "1.500000000000" {
		t.Fatalf("current local day did not include its completed observation: %+v", lastBucket)
	}
	totals := live["totals"].(map[string]any)
	if live["availability"] != "partial" || totals["energy"].(map[string]any)["value"] != "1.500000000000" || totals["cost"] != nil || totals["waste_energy"] != nil {
		t.Fatalf("unsupported value fabricated: %+v", live)
	}
	if live["snapshot_id"] == empty["snapshot_id"] {
		t.Fatal("snapshot unchanged after new evidence")
	}
	withoutTariffSnapshot := live["snapshot_id"]
	if code, replaced := getSample(dash, "owner"); code != 200 || replaced["data_mode"] != "mixed" || replaced["totals"].(map[string]any)["cost"] != nil || len(replaced["device_allocations"].([]any)) != 19 {
		t.Fatalf("real evidence did not retain separate seeded allocations: %d %+v", code, replaced)
	}
	national := s
	national.NationalRate = func(context.Context) (float64, error) { return 0.2, nil }
	nationalRouter := chi.NewRouter()
	nationalRouter.Route("/v1", national.Routes)
	nationalReq := httptest.NewRequest("GET", dash, nil)
	nationalReq.Header.Set("Authorization", "Bearer owner")
	nationalW := httptest.NewRecorder()
	nationalRouter.ServeHTTP(nationalW, nationalReq)
	var nationalDash map[string]any
	_ = json.Unmarshal(nationalW.Body.Bytes(), &nationalDash)
	nationalTotals := nationalDash["totals"].(map[string]any)
	nationalReal := nationalDash["device_allocations"].([]any)[18].(map[string]any)["allocation"].(map[string]any)
	if nationalTotals["cost"].(map[string]any)["amount"] != "0.300000" || nationalTotals["rate_basis"] != "us_average" || nationalReal["cost"].(map[string]any)["amount"] != "0.300000" || nationalReal["rate_basis"] != "us_average" {
		t.Fatalf("no-tariff home was not priced at the national rate: %+v", nationalDash)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','owner bill',$2,false,$3)`, home.ID, now.Add(-10*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, priced := get(dash, "owner")
	if code != 200 {
		t.Fatalf("priced dashboard: %d %+v", code, priced)
	}
	pricedTotals := priced["totals"].(map[string]any)
	if pricedTotals["cost"].(map[string]any)["amount"] != "0.300000" || pricedTotals["unclassified_cost"].(map[string]any)["amount"] != "0.300000" || pricedTotals["useful_cost"] != nil || pricedTotals["waste_cost"] != nil || priced["evidence_basis"] != "mixed" {
		t.Fatalf("measured cost contract mismatch: %+v", priced)
	}
	pricedReal := priced["device_allocations"].([]any)[18].(map[string]any)["allocation"].(map[string]any)
	if pricedReal["cost"].(map[string]any)["amount"] != "0.300000" || pricedReal["unclassified_cost"].(map[string]any)["amount"] != "0.300000" || pricedReal["useful_cost"] != nil || pricedReal["waste_cost"] != nil {
		t.Fatalf("measured appliance pricing or classification mismatch: %+v", pricedReal)
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
	awaySource := func() any {
		code, r := get(mtdTrend, "owner")
		eff, ok := r["efficiency_month_to_date"].(map[string]any)
		if code != 200 || !ok {
			t.Fatalf("measured MTD efficiency missing: %d %+v", code, r)
		}
		return eff["away_source"]
	}
	if got := awaySource(); got != "default_schedule" {
		t.Fatalf("home without a schedule away_source = %v", got)
	}
	if _, err = pool.Exec(ctx, `UPDATE home_preferences SET away_schedule='[{"days":["mon"],"start_local":"10:00","end_local":"12:00"}]'::jsonb WHERE home_id=$1`, home.ID); err != nil {
		t.Fatal(err)
	}
	if got := awaySource(); got != "user_schedule" {
		t.Fatalf("saved schedule away_source = %v", got)
	}
	if _, err = pool.Exec(ctx, `UPDATE home_preferences SET away_schedule=NULL WHERE home_id=$1`, home.ID); err != nil {
		t.Fatal(err)
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
	if correctedTotals["energy"].(map[string]any)["value"] != "0.500000000000" || correctedTotals["cost"].(map[string]any)["amount"] != "0.100000" {
		t.Fatalf("corrected observation was not priced once: %+v", corrected)
	}
	code, correctedSeries := get(trend, "owner")
	if code != 200 || correctedSeries["snapshot_id"] != corrected["snapshot_id"] || !reflect.DeepEqual(correctedSeries["period"], corrected["period"]) || correctedSeries["scope"] != corrected["scope"] {
		t.Fatalf("corrected views diverged: %d %+v", code, correctedSeries)
	}
	seriesTotals := correctedSeries["totals"].(map[string]any)
	if seriesTotals["energy"].(map[string]any)["value"] != "0.500000000000" || seriesTotals["cost"].(map[string]any)["amount"] != "0.100000" {
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
	// This correction crosses an hourly bucket boundary. Both adjacent hours
	// receive their proportional share and the total remains unchanged.
	straddlingStart := start.Add(-30 * time.Second)
	straddlingEnd := end.Add(-30 * time.Second)
	correction.Revision = 3
	correction.Start = &straddlingStart
	correction.End = &straddlingEnd
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{correction}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, allocated := get(dash, "owner")
	if code != 200 || allocated["snapshot_id"] == corrected["snapshot_id"] {
		t.Fatalf("boundary correction did not change snapshot: %d %+v", code, allocated)
	}
	code, allocatedSeries := get(trend, "owner")
	if code != 200 || allocatedSeries["snapshot_id"] != allocated["snapshot_id"] || !reflect.DeepEqual(allocatedSeries["period"], allocated["period"]) || allocatedSeries["scope"] != allocated["scope"] {
		t.Fatalf("boundary allocated views diverged: %d %+v", code, allocatedSeries)
	}
	for _, response := range []map[string]any{allocated, allocatedSeries} {
		totals := response["totals"].(map[string]any)
		if totals["energy"].(map[string]any)["value"] != "0.500000000000" || totals["cost"].(map[string]any)["amount"] != "0.100000" || response["availability"] != "partial" {
			t.Fatalf("boundary interval total was lost or changed: %+v", response)
		}
	}
	var seamEnergy, seamCost big.Rat
	for _, raw := range allocatedSeries["buckets"].([]any) {
		part := raw.(map[string]any)["allocation"].(map[string]any)
		if part["energy"] != nil {
			v, _ := new(big.Rat).SetString(part["energy"].(map[string]any)["value"].(string))
			seamEnergy.Add(&seamEnergy, v)
		}
		if part["cost"] != nil {
			v, _ := new(big.Rat).SetString(part["cost"].(map[string]any)["amount"].(string))
			seamCost.Add(&seamCost, v)
		}
	}
	if seamEnergy.Cmp(big.NewRat(1, 2)) != 0 || seamCost.Cmp(big.NewRat(1, 10)) != 0 {
		t.Fatalf("boundary buckets do not reconcile: energy=%s cost=%s", &seamEnergy, &seamCost)
	}
	correction.Revision = 4
	correction.Start = &start
	correction.End = &end
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "live-energy-cert", 1, telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{correction}}, hash[:]); err != nil {
		t.Fatal(err)
	}
	code, restored := get(dash, "owner")
	if code != 200 || restored["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != "0.100000" {
		t.Fatalf("restored measurement was not priced: %d %+v", code, restored)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,2,'electricity',0.25,'kWh','USD','user_tariff','updated bill',$2,false,$3)`, home.ID, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, revised := get(dash, "owner")
	if code != 200 || revised["snapshot_id"] == restored["snapshot_id"] || revised["totals"].(map[string]any)["cost"].(map[string]any)["amount"] != "0.100000" {
		t.Fatalf("historically effective tariff was not retained after new revision: %d %+v", code, revised)
	}
	code, revisedSeries := get(trend, "owner")
	if code != 200 || revisedSeries["snapshot_id"] != revised["snapshot_id"] || !reflect.DeepEqual(revisedSeries["period"], revised["period"]) || revisedSeries["scope"] != revised["scope"] {
		t.Fatalf("effective tariff views diverged: %d %+v", code, revisedSeries)
	}
	for _, response := range []map[string]any{revised, revisedSeries} {
		total := response["totals"].(map[string]any)
		if total["cost"].(map[string]any)["amount"] != "0.100000" || total["unclassified_cost"].(map[string]any)["amount"] != "0.100000" || total["useful_energy"] != nil || total["useful_cost"] != nil || total["waste_energy"] != nil || total["waste_cost"] != nil {
			t.Fatalf("effective tariff cost or classification mismatch: %+v", response)
		}
	}
	// The tariff expires at exactly the electricity max age; test the strict freshness
	// boundary directly so DST cannot turn it into calendar-day arithmetic.
	staleHome, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "Stale tariff", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	boundary := now.Add(-tariffpkg.ElectricityMaxAge)
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.20,'kWh','USD','user_tariff','expired bill',$2,false,$3)`, staleHome.ID, boundary, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The strict 90-day freshness window includes the instant before the
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
		t.Fatalf("tariff was not selected before 90-day boundary: id=%q revision=%d tariff=%+v err=%v", latestID, latestRevision, selected, err)
	}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	latestID, latestRevision, selected, err = readMarginalTariff(ctx, tx, staleHome.ID, now, items)
	_ = tx.Rollback(ctx)
	if err != nil || latestID == "" || latestRevision != 1 || selected != nil {
		t.Fatalf("expired tariff was selected at exact 90-day boundary: id=%q revision=%d tariff=%+v err=%v", latestID, latestRevision, selected, err)
	}
	// A home whose only tariff is older than the max age is priced at the
	// national rate; a younger tariff stays the home schedule.
	flatNational := NationalRate(func(context.Context) (float64, error) { return 0.2, nil })
	for _, test := range []struct {
		name      string
		age       time.Duration
		wantBasis string
		wantRate  *big.Rat
	}{
		{"91 days old", 91 * 24 * time.Hour, "us_average", big.NewRat(1, 5)},
		{"89 days old", 89 * 24 * time.Hour, "", big.NewRat(3, 10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ageHome, createErr := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: test.name, Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
			if createErr != nil {
				t.Fatal(createErr)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.30,'kWh','USD','user_tariff','owner bill',$2,false,$3)`, ageHome.ID, now.Add(-test.age), now.Add(-120*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, _, schedule, err := readMarginalTariff(ctx, tx, ageHome.ID, now, items)
			if err != nil {
				t.Fatal(err)
			}
			if schedule == nil {
				schedule = flatNational.scheduleFor(ctx, items)
			}
			if schedule == nil || schedule.nationalBasis() != test.wantBasis || len(schedule.Segments) == 0 || schedule.Segments[0].Rate.Cmp(test.wantRate) != 0 {
				t.Fatalf("wrong schedule for tariff aged %s: %+v", test.age, schedule)
			}
		})
	}
	// A tariff that ended before the read can still conflict with a current
	// tariff over the measured interval. Adjacent half-open intervals do not.
	for _, test := range []struct {
		name string
		end  time.Time
		want bool
	}{
		{"overlapping historical revision", end.Add(time.Minute), true},
		{"adjacent historical revision", start, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tariffHome, createErr := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: test.name, Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
			if createErr != nil {
				t.Fatal(createErr)
			}
			for revision, dates := range []struct{ from, to time.Time }{
				{now.Add(-24 * time.Hour), now.Add(time.Hour)},
				{start.Add(-time.Hour), test.end},
			} {
				if _, createErr = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from,effective_to) VALUES($1,$2,'electricity',0.20,'kWh','USD','user_tariff','reviewed bill',$3,false,$4,$5)`, tariffHome.ID, revision+1, now.Add(-24*time.Hour), dates.from, dates.to); createErr != nil {
					t.Fatal(createErr)
				}
			}
			readTx, readErr := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if readErr != nil {
				t.Fatal(readErr)
			}
			_, _, rate, readErr := readMarginalTariff(ctx, readTx, tariffHome.ID, now, items)
			_ = readTx.Rollback(ctx)
			if readErr != nil || (rate != nil) != test.want {
				t.Fatalf("selected rate = %+v, want selection %v: %v", rate, test.want, readErr)
			}
		})
	}
	t.Run("adjacent effective rates split one observation", func(t *testing.T) {
		tariffHome, createErr := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "Rate transition", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
		if createErr != nil {
			t.Fatal(createErr)
		}
		middle := start.Add(end.Sub(start) / 2)
		for _, row := range []struct {
			revision int
			rate     string
			from     time.Time
			to       *time.Time
		}{{1, "0.10", start.Add(-time.Hour), &middle}, {2, "0.20", middle, nil}} {
			if _, createErr = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from,effective_to) VALUES($1,$2,'electricity',$3,'kWh','USD','user_tariff','reviewed bill',$4,false,$5,$6)`, tariffHome.ID, row.revision, row.rate, now.Add(-24*time.Hour), row.from, row.to); createErr != nil {
				t.Fatal(createErr)
			}
		}
		readTx, readErr := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if readErr != nil {
			t.Fatal(readErr)
		}
		_, _, schedule, readErr := readMarginalTariff(ctx, readTx, tariffHome.ID, now, items)
		_ = readTx.Rollback(ctx)
		if readErr != nil || schedule == nil {
			t.Fatalf("adjacent rates were not selected: %+v %v", schedule, readErr)
		}
		buckets := makeBuckets(period{Start: start, End: end}, "hour", items)
		if cost := priceEffectiveBuckets(buckets, items, *schedule); cost == nil || cost.Amount != "0.225000" {
			t.Fatalf("mid-interval tariff price = %+v", cost)
		}
	})
	if code, _ := get("/v1/homes/"+home.ID+"/energy-trends?granularity=invalid", "owner"); code != 400 {
		t.Fatalf("invalid granularity: %d", code)
	}
	if code, _ := get("/v1/homes/"+home.ID+"/energy-trends?granularity=hour&window=trailing_30_days", "owner"); code != 400 {
		t.Fatalf("trailing window accepted non-day granularity: %d", code)
	}
	now = now.AddDate(0, 0, 1)
	code, stale := get(dash, "owner")
	if code != 200 || stale["totals"].(map[string]any)["energy"] != nil || stale["as_of"] != end.UTC().Format(time.RFC3339) {
		t.Fatalf("prior-day evidence leaked into new period: %d %+v", code, stale)
	}
	if code, replaced := getSample(dash, "owner"); code != 200 || replaced["data_mode"] != "mixed" || replaced["totals"].(map[string]any)["energy"] != nil {
		t.Fatalf("historical evidence lost mixed mode: %d %+v", code, replaced)
	}
}
