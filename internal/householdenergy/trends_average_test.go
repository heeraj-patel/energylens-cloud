package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

// TestLast5OccurrencesTotalIsAveragedPerGranularity covers ELR-044: Daily,
// Weekly, and Monthly Trends must show the average Useful/Waste per elapsed
// day/week/month in the period, not the period's running sum.
func TestLast5OccurrencesTotalIsAveragedPerGranularity(t *testing.T) {
	fuel := "electricity"
	for _, tc := range []struct {
		name          string
		granularity   string
		periodStart   time.Time
		periodEnd     time.Time
		itemEnergyWh  []float64
		wantPeriods   int
		wantTotalKWh  string
		wantAvgKWh    string
		tariffRateUSD *big.Rat
		wantAvgCost   string
	}{
		{
			name:         "day",
			granularity:  "day",
			periodStart:  time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
			periodEnd:    time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
			itemEnergyWh: []float64{1000, 2000, 3000},
			wantPeriods:  3,
			wantTotalKWh: "6",
			wantAvgKWh:   "2",
		},
		{
			name:         "week",
			granularity:  "week",
			periodStart:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			periodEnd:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			itemEnergyWh: []float64{7000, 14000},
			wantPeriods:  2,
			wantTotalKWh: "21",
			wantAvgKWh:   "10.5",
		},
		{
			name:         "month",
			granularity:  "month",
			periodStart:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			periodEnd:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			itemEnergyWh: []float64{30000, 60000, 90000, 120000, 150000},
			wantPeriods:  5,
			wantTotalKWh: "450",
			wantAvgKWh:   "90",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := period{Start: tc.periodStart, End: tc.periodEnd}
			// Only the first len(itemEnergyWh) buckets get an observation, as
			// if the home only has that many elapsed days/weeks/months of
			// history so far; later buckets in the period stay empty.
			emptyBuckets := makeBuckets(p, tc.granularity, nil)
			var items []interval
			for i, wh := range tc.itemEnergyWh {
				b := emptyBuckets[i]
				items = append(items, interval{Start: b.Start, End: b.End, Wh: big.NewRat(int64(wh), 1), Fuel: &fuel, ApplianceID: "a", SensorKind: "plug"})
			}
			buckets := makeBuckets(p, tc.granularity, items)

			periods := countPeriodsWithData(buckets)
			if periods != tc.wantPeriods {
				t.Fatalf("periodsWithData = %d, want %d", periods, tc.wantPeriods)
			}

			var total *big.Rat
			for _, b := range buckets {
				if b.energyKWh != nil {
					if total == nil {
						total = new(big.Rat)
					}
					total.Add(total, b.energyKWh)
				}
			}
			if total == nil || total.Cmp(mustDecimal(tc.wantTotalKWh)) != 0 {
				t.Fatalf("raw total kWh = %v, want %s", total, tc.wantTotalKWh)
			}

			averaged := averageOverPeriods(total, periods)
			want := mustDecimal(tc.wantAvgKWh)
			if averaged.Cmp(want) != 0 {
				t.Fatalf("averaged total kWh = %s, want %s", averaged.FloatString(6), tc.wantAvgKWh)
			}

			rate := marginalTariff{Rate: big.NewRat(1, 5), Currency: "USD", From: tc.periodStart.Add(-time.Hour)}
			schedule := marginalTariff{Currency: "USD", Segments: []marginalTariff{rate}}
			cost := priceEffectiveBuckets(buckets, items, schedule)
			if cost == nil {
				t.Fatal("priceEffectiveBuckets returned nil")
			}
			avgCost := averageOverMoney(*cost, periods)
			wantAvgCost := new(big.Rat).Quo(mustDecimal(cost.Amount), big.NewRat(int64(periods), 1))
			if avgCost.Amount != wantAvgCost.FloatString(6) {
				t.Fatalf("averaged cost = %s, want %s (raw total cost %s)", avgCost.Amount, wantAvgCost.FloatString(6), cost.Amount)
			}
		})
	}
}

func TestCountPeriodsWithDataIgnoresEmptyBuckets(t *testing.T) {
	buckets := []bucket{{energyKWh: big.NewRat(1, 1)}, {energyKWh: nil}, {energyKWh: big.NewRat(2, 1)}}
	if got := countPeriodsWithData(buckets); got != 2 {
		t.Fatalf("countPeriodsWithData = %d, want 2", got)
	}
}

func TestAverageOverPeriodsNoOpWhenNothingToDivide(t *testing.T) {
	if got := averageOverPeriods(nil, 5); got != nil {
		t.Fatalf("averaging a nil total should stay nil, got %v", got)
	}
	total := big.NewRat(10, 1)
	if got := averageOverPeriods(total, 0); got != total {
		t.Fatalf("zero periods should leave the total unchanged, got %v", got)
	}
}
