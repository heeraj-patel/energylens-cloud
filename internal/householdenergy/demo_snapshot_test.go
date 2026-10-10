package householdenergy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func demoSnapshotForTest() DemoCostSnapshot {
	return MobileDemoCostSnapshot()
}

func TestDemoCostSnapshotValidationAndDecode(t *testing.T) {
	s := demoSnapshotForTest()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDemoCostSnapshot(strings.NewReader(string(data)))
	if err != nil || len(decoded.Devices) != 18 {
		t.Fatalf("decode snapshot: %v", err)
	}
	s.Devices = s.Devices[:17]
	if err := s.Validate(); err == nil {
		t.Fatal("accepted fewer than 18 device leaves")
	}
	s = demoSnapshotForTest()
	s.AdjustmentLocalDate = "2026-09-29"
	s.Unattributed.Adjustment = nil
	if err := s.Validate(); err == nil {
		t.Fatal("accepted adjustment date without adjustment cost")
	}
}

func TestDeviceAllocationMarshalsSyntheticNullableIdentityWithoutChangingRealShape(t *testing.T) {
	a := allocation{Availability: "unavailable", EvidenceBasis: "synthetic"}
	synthetic, err := json.Marshal(deviceAllocation{DeviceID: "demo-device-11", Name: nil, Allocation: a})
	if err != nil || !strings.Contains(string(synthetic), `"name":null`) || strings.Contains(string(synthetic), `"appliance_id"`) {
		t.Fatalf("bad synthetic allocation JSON: %s (%v)", synthetic, err)
	}
	real, err := json.Marshal(deviceAllocation{ApplianceID: "10000000-0000-4000-8000-000000000001", Allocation: a})
	if err != nil || strings.Contains(string(real), `"name"`) || strings.Contains(string(real), `"device_id"`) {
		t.Fatalf("real allocation JSON changed shape: %s (%v)", real, err)
	}
}

func TestDemoSnapshotReferenceSplitAndDailyAdjustment(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, loc)
	now := start.Add(12 * time.Hour)
	rows, total, devices, unattr := projectDemoSnapshot(s, period{start, start.AddDate(0, 0, 1)}, "day", now)
	if len(rows) != 1 || len(devices) != 18 || unattr == nil {
		t.Fatalf("unexpected snapshot projection: %d %d %+v", len(rows), len(devices), unattr)
	}
	if total.Cost.(money).Amount != "4.920000" || total.UsefulCost.(money).Amount != "3.480000" || total.WasteCost.(money).Amount != "1.440000" {
		t.Fatalf("reference day split mismatch: %+v", total)
	}
	if total.Energy != nil || total.UnclassifiedCost != nil || total.EvidenceBasis != "synthetic" {
		t.Fatalf("cost-only source inferred energy or classification: %+v", total)
	}
	if rows[0].DeviceAllocations[10].Name != nil || rows[0].DeviceAllocations[0].DeviceID != "demo-device-01" || rows[0].DeviceAllocations[0].ApplianceID != "" {
		t.Fatalf("synthetic device identity mismatch: %+v", rows[0].DeviceAllocations[0])
	}
	prev := start.AddDate(0, 0, -1)
	adjusted, adjustedTotal, _, adjustedUnattr := projectDemoSnapshot(s, period{prev, prev.AddDate(0, 0, 1)}, "day", now)
	if len(adjusted) != 1 || adjustedTotal.WasteCost.(money).Amount != "0.780000" || adjustedUnattr == nil || adjustedUnattr.WasteCost.(money).Amount != "0.380000" {
		t.Fatalf("adjustment-day cost mismatch: %+v", adjustedTotal)
	}
}

func TestDemoSnapshotMonthToDateCostIncludesPartialCurrentDay(t *testing.T) {
	s := demoSnapshotForTest()
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, loc)
	p := monthToDatePeriod(now, loc)
	buckets, total, _, _ := projectDemoSnapshot(s, p, "day", now)
	if len(buckets) != 2 || total.Cost.(money).Amount != "6.840000" ||
		total.UsefulCost.(money).Amount != "6.000000" || total.WasteCost.(money).Amount != "0.840000" {
		t.Fatalf("month-to-date cost did not reconcile with current-day fraction: %+v (%d buckets)", total, len(buckets))
	}
	if buckets[0].Allocation.Cost.(money).Amount != "4.560000" || buckets[1].Allocation.Cost.(money).Amount != "2.280000" {
		t.Fatalf("unexpected daily costs: %+v", buckets)
	}
}

func TestDemoSnapshotHourlyCostsReconcileAndProrateLocalDay(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, loc)
	now := start.Add(12*time.Hour + 30*time.Minute)
	buckets, total, devices, unattr := projectDemoSnapshot(s, period{start, start.AddDate(0, 0, 1)}, "hour", now)
	if len(buckets) != 24 || len(devices) != 18 || unattr == nil {
		t.Fatalf("unexpected hourly projection: %d/%d", len(buckets), len(devices))
	}
	var sum int64
	for _, b := range buckets {
		if b.Start.Before(now) {
			if b.Allocation.Energy != nil || len(b.DeviceAllocations) != 18 || b.Unattributed == nil {
				t.Fatalf("hourly synthetic row lost identity or invented energy: %+v", b)
			}
			sum += moneyMicrosUnits(b.Allocation.Cost)
		} else if b.Allocation.Availability != "unavailable" {
			t.Fatalf("future hour available: %+v", b)
		}
	}
	if sum != moneyMicrosUnits(total.Cost) {
		t.Fatalf("hourly buckets %d != top-level %d", sum, moneyMicrosUnits(total.Cost))
	}
	if got := moneyMicrosUnits(total.Cost); got < 5_124_000 || got > 5_126_000 {
		t.Fatalf("12.5-hour allocation was %d micros", got)
	}
	closed, closedTotal, _, _ := projectDemoSnapshot(s, period{start, start.AddDate(0, 0, 1)}, "hour", start.AddDate(0, 0, 1))
	var closedSum int64
	for _, b := range closed {
		closedSum += moneyMicrosUnits(b.Allocation.Cost)
	}
	if closedSum != 9_840_000 || moneyMicrosUnits(closedTotal.Cost) != closedSum {
		t.Fatalf("full local day did not exactly reconcile across hours: bucket sum=%d total=%d", closedSum, moneyMicrosUnits(closedTotal.Cost))
	}
}

func TestDemoSnapshotDailyWeeklyMonthlyAvailableAtPeriodBoundaries(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	for _, boundary := range []time.Time{
		time.Date(2026, 10, 5, 0, 0, 0, 0, loc), // Monday/week start
		time.Date(2026, 10, 1, 0, 0, 0, 0, loc), // month start
	} {
		for _, granularity := range []string{"day", "week", "month"} {
			t.Run(boundary.Format("2006-01-02")+"/"+granularity, func(t *testing.T) {
				p := periodFor(boundary, loc, granularity)
				buckets, total, devices, unattributed := projectDemoSnapshot(s, p, granularity, boundary)
				if total.Cost == nil || total.Availability != "partial" || total.EvidenceBasis != "synthetic" || len(devices) != 18 || unattributed == nil {
					t.Fatalf("aggregate is unavailable at boundary: total=%+v devices=%d unattributed=%+v", total, len(devices), unattributed)
				}
				populated := false
				for _, b := range buckets {
					if b.Allocation.Cost == nil {
						continue
					}
					populated = true
					if len(b.DeviceAllocations) != 18 || b.Unattributed == nil {
						t.Fatalf("bucket lacks 18 unique daily aggregate rows: %+v", b)
					}
					seen := map[string]bool{}
					for _, d := range b.DeviceAllocations {
						if seen[d.DeviceID] {
							t.Fatalf("duplicate device row in aggregate bucket: %s", d.DeviceID)
						}
						seen[d.DeviceID] = true
					}
				}
				if !populated {
					t.Fatal("current period has no available bucket")
				}
				if total.Energy != nil {
					t.Fatal("cost-only period invented energy")
				}
			})
		}
	}
}

func TestLastFiveMockOccurrencesAlwaysHaveDeviceAndAggregateCosts(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	for _, now := range []time.Time{
		time.Date(2026, 3, 10, 12, 0, 0, 0, loc), // crosses DST and month for weekly views
		time.Date(2026, 10, 5, 0, 0, 0, 0, loc),  // current week at midnight
		time.Date(2026, 1, 1, 0, 0, 0, 0, loc),   // current month/year boundary
	} {
		for _, granularity := range []string{"day", "week", "month"} {
			p := last5OccurrencesPeriod(now, loc, granularity)
			buckets, total, devices, unattributed := projectDemoSnapshot(s, p, granularity, now)
			if len(buckets) != 5 || len(devices) != 18 || total.Cost == nil || total.Availability != "partial" || unattributed == nil {
				t.Fatalf("%s at %s lacks a valid five-occurrence aggregate: buckets=%d devices=%d total=%+v", granularity, now, len(buckets), len(devices), total)
			}
			for i, b := range buckets {
				if b.Allocation.Cost == nil || len(b.DeviceAllocations) != 18 || b.Unattributed == nil {
					t.Fatalf("%s occurrence %d at %s unavailable: %+v", granularity, i, now, b)
				}
				if b.Allocation.Energy != nil {
					t.Fatalf("%s occurrence invented energy", granularity)
				}
			}
		}
	}
}

func TestDemoSnapshotZeroElapsedHourIsAvailableAndSynthetic(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	p := periodFor(now, loc, "hour")
	buckets, total, devices, unattributed := projectDemoSnapshot(s, p, "hour", now)
	if len(buckets) != 24 || len(devices) != 18 || unattributed == nil || total.Cost.(money).Amount != "0.000000" || total.Availability != "partial" || total.EvidenceBasis != "synthetic" {
		t.Fatalf("midnight dashboard/hour projection unavailable: total=%+v devices=%d unattributed=%+v", total, len(devices), unattributed)
	}
	if len(buckets[0].DeviceAllocations) != 18 || buckets[0].Allocation.Cost.(money).Amount != "0.000000" || buckets[0].Allocation.Availability != "partial" {
		t.Fatalf("zero elapsed current hour lacks synthetic zero rows: %+v", buckets[0])
	}
	if buckets[1].Allocation.Availability != "unavailable" {
		t.Fatalf("future hour is not unavailable: %+v", buckets[1])
	}
}

func TestDemoSnapshotDailySummaryAccruesCurrentDay(t *testing.T) {
	s := demoSnapshotForTest()
	loc, _ := time.LoadLocation(s.Timezone)
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, loc)
	_, total, _, _ := projectDemoSnapshot(s, period{start, start.AddDate(0, 0, 1)}, "day", start.Add(2*time.Hour))
	if got := total.Cost.(money).Amount; got != "0.379976" {
		t.Fatalf("current daily summary should accrue only elapsed local time, got %s", got)
	}
}
