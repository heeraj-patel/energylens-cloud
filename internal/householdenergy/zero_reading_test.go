package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

func TestMeteredApplianceRowsForZeroNoneAndNonzeroReadings(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	fuel := "electricity"
	now := end.Add(time.Hour)
	segment := marginalTariff{Rate: big.NewRat(1, 10), Currency: "USD", From: start.Add(-time.Hour), SourceAsOf: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name    string
		wh      *big.Rat
		want    bool
		energy  string
		cost    string
		measure string
	}{
		{"zero", big.NewRat(0, 1), true, "0.000000000000", "0.000000", "mixed"},
		{"none", nil, false, "", "", ""},
		{"nonzero", big.NewRat(1000, 1), true, "1.000000000000", "0.100000", "mixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []interval{}
			if tc.wh != nil {
				items = append(items, interval{Start: start, End: end, Wh: tc.wh, Fuel: &fuel, ApplianceID: "dyson", SensorKind: "plug"})
			}
			schedule := selectTariffSchedule(now, []interval{{Start: start, End: end, Wh: big.NewRat(1, 1), Fuel: &fuel}}, []marginalTariff{segment})
			if schedule == nil {
				t.Fatal("tariff not selected")
			}
			rate := *schedule
			buckets := makeBuckets(period{Start: start, End: end}, "hour", items)
			rows, wh := attributeDeviceBuckets(buckets, items, nil)
			periodRows := sumDeviceAllocations(wh, nil)
			priceAttributedDevices(buckets, rows, periodRows, items, rate)
			if !tc.want {
				if len(rows[0]) != 0 || len(periodRows) != 0 {
					t.Fatalf("rows without readings: %+v %+v", rows, periodRows)
				}
				return
			}
			if len(rows[0]) != 1 || len(periodRows) != 1 {
				t.Fatalf("rows = %+v period = %+v", rows, periodRows)
			}
			got := rows[0][0].Allocation
			if got.Energy.Value != tc.energy || got.Cost.(money).Amount != tc.cost || got.EvidenceBasis != tc.measure {
				t.Fatalf("allocation = %+v", got)
			}
		})
	}
}
