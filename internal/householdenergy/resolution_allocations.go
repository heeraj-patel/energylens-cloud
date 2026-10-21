package householdenergy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"energylens.local/cloud/internal/energyrate"
	"github.com/jackc/pgx/v5"
)

type resolvedEnergy struct {
	Status     string `json:"status"`
	DeviceKind string `json:"device_kind"`
	Values     *struct {
		AnnualKWh      *float64 `json:"annual_kwh"`
		KWhPerDay      *float64 `json:"kwh_per_day"`
		WasteKWhPerDay *float64 `json:"waste_kwh_per_day"`
		WasteBasis     string   `json:"waste_basis"`
	} `json:"values"`
	WasteFloor *struct {
		USDPerDay *float64 `json:"usd_per_day"`
	} `json:"waste_floor"`
}

// resolutionDeviceRows keep estimates beside measured allocations. They never
// enter whole-home measured totals, which may already include these loads.
// Only electric appliances count: a gas appliance's record is never electric kWh.
func resolutionDeviceRows(ctx context.Context, tx pgx.Tx, home string, buckets []bucket, now time.Time, national NationalRate) ([][]deviceAllocation, []deviceAllocation, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text,r.record FROM appliances a JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||a.id::text AND r.record->'appliance_version'=to_jsonb(a.version) WHERE a.home_id=$1 AND a.archived_at IS NULL AND COALESCE(a.identity->>'fuel','electricity')='electricity' AND NOT EXISTS(SELECT 1 FROM attachments t JOIN sensor_endpoints s ON s.id=t.sensor_id WHERE t.appliance_id=a.id AND t.ended_at IS NULL AND s.archived_at IS NULL AND s.capabilities @> ARRAY['power','energy']::text[]) ORDER BY a.id`, home)
	if err != nil {
		return nil, nil, err
	}
	type device struct {
		id         string
		resolution resolvedEnergy
	}
	devices := []device{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var record resolvedEnergy
		if err = json.Unmarshal(raw, &record); err != nil {
			break
		}
		if record.estimable() {
			devices = append(devices, device{id, record})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	byBucket := make([][]deviceAllocation, len(buckets))
	period := []deviceAllocation{}
	if len(devices) == 0 || len(buckets) == 0 {
		return byBucket, period, nil
	}
	electricity := "electricity"
	tariff, rateBasis, err := estimateElectricSchedule(ctx, tx, home, now, buckets, national)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range devices {
		var total, allWaste, totalCost, allWasteCost big.Rat
		priced, wasteKnown := tariff != nil, true
		for bi, b := range buckets {
			rate := 0.0
			if tariff != nil {
				rate = bucketRate(*tariff, b)
			}
			energy, waste, known := resolutionKWhForPeriod(d.resolution, b.Start, b.End, rate)
			if energy <= 0 || math.IsNaN(energy) || math.IsInf(energy, 0) {
				continue
			}
			e := mustDecimal(fmt.Sprintf("%.9f", energy))
			total.Add(&total, e)
			a := fromKWh(e)
			a.EvidenceBasis = "estimated"
			a.WasteBasis, a.RateBasis = d.resolution.wasteBasis(), rateBasis
			w := mustDecimal(fmt.Sprintf("%.9f", waste))
			if known {
				allWaste.Add(&allWaste, w)
				a.WasteEnergy = &quantity{Value: w.FloatString(12), Unit: "kWh"}
				a.UsefulEnergy = &quantity{Value: nonNegative(new(big.Rat).Sub(e, w)).FloatString(12), Unit: "kWh"}
				a.UnclassifiedEnergy = nil
			} else {
				wasteKnown = false
			}
			if tariff != nil {
				piece := interval{Start: b.Start, End: b.End, Wh: new(big.Rat).Mul(e, big.NewRat(1000, 1)), Fuel: &electricity}
				cost := priceIntervalRange(piece, b.Start, b.End, *tariff)
				if cost == nil {
					priced = false
				} else {
					totalCost.Add(&totalCost, cost)
					a.Cost = money{Amount: cost.FloatString(6), Currency: tariff.Currency}
					a.UnclassifiedCost = a.Cost
				}
				if cost != nil && known {
					piece.Wh = new(big.Rat).Mul(w, big.NewRat(1000, 1))
					wc := priceIntervalRange(piece, b.Start, b.End, *tariff)
					if wc == nil {
						priced = false
					} else {
						allWasteCost.Add(&allWasteCost, wc)
						a.WasteCost = money{Amount: wc.FloatString(6), Currency: tariff.Currency}
						a.UsefulCost = money{Amount: nonNegative(new(big.Rat).Sub(cost, wc)).FloatString(6), Currency: tariff.Currency}
						a.UnclassifiedCost = nil
					}
				}
			}
			byBucket[bi] = append(byBucket[bi], deviceAllocation{ApplianceID: d.id, Allocation: a})
		}
		if total.Sign() == 0 {
			continue
		}
		a := fromKWh(&total)
		a.EvidenceBasis = "estimated"
		a.WasteBasis, a.RateBasis = d.resolution.wasteBasis(), rateBasis
		if wasteKnown {
			a.WasteEnergy = &quantity{Value: allWaste.FloatString(12), Unit: "kWh"}
			a.UsefulEnergy = &quantity{Value: nonNegative(new(big.Rat).Sub(&total, &allWaste)).FloatString(12), Unit: "kWh"}
			a.UnclassifiedEnergy = nil
		}
		if priced {
			a.Cost = money{Amount: totalCost.FloatString(6), Currency: tariff.Currency}
			a.UnclassifiedCost = a.Cost
			if wasteKnown {
				a.WasteCost = money{Amount: allWasteCost.FloatString(6), Currency: tariff.Currency}
				a.UsefulCost = money{Amount: nonNegative(new(big.Rat).Sub(&totalCost, &allWasteCost)).FloatString(6), Currency: tariff.Currency}
				a.UnclassifiedCost = nil
			}
		}
		period = append(period, deviceAllocation{ApplianceID: d.id, Allocation: a})
	}
	return byBucket, period, nil
}

// estimateElectricSchedule is the electricity schedule that prices estimated
// rows: the home tariff, else the national rate; rateBasis is empty when neither.
func estimateElectricSchedule(ctx context.Context, tx pgx.Tx, home string, now time.Time, buckets []bucket, national NationalRate) (*marginalTariff, string, error) {
	electricity := "electricity"
	probe := interval{Start: buckets[0].Start, End: buckets[len(buckets)-1].End, Wh: big.NewRat(1, 1), Fuel: &electricity}
	_, _, tariff, err := readEffectiveTariff(ctx, tx, home, now, []interval{probe})
	if err != nil {
		return nil, "", err
	}
	rateBasis := "home_tariff"
	if tariff == nil {
		tariff, rateBasis = national.schedule(ctx), "us_average"
	}
	if tariff == nil {
		rateBasis = ""
	}
	return tariff, rateBasis, nil
}

// resolutionKWhForPeriod returns the period's usage and waste; waste is not
// known when a floor record has no rate to convert it with.
func resolutionKWhForPeriod(record resolvedEnergy, start, end time.Time, rate float64) (float64, float64, bool) {
	var energy, waste float64
	known := true
	for at := start; at.Before(end); {
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		next := day.AddDate(0, 0, 1)
		stop := end
		if next.Before(stop) {
			stop = next
		}
		fraction := stop.Sub(at).Seconds() / next.Sub(day).Seconds()
		daily := 0.0
		if record.Values.KWhPerDay != nil {
			daily = *record.Values.KWhPerDay
		} else if record.Values.AnnualKWh != nil {
			days := 365.0
			if day.Year()%4 == 0 && (day.Year()%100 != 0 || day.Year()%400 == 0) {
				days = 366
			}
			daily = *record.Values.AnnualKWh / days
		}
		energy += daily * fraction
		dayWaste, ok := record.wasteKWhPerDay(daily, rate)
		known = known && ok
		waste += dayWaste * fraction
		at = stop
	}
	return energy, waste, known
}

// bucketRate is the bucket's average tariff rate in $/kWh.
func bucketRate(tariff marginalTariff, b bucket) float64 {
	unit := interval{Start: b.Start, End: b.End, Wh: big.NewRat(1000, 1)}
	if cost := priceIntervalRange(unit, b.Start, b.End, tariff); cost != nil {
		rate, _ := cost.Float64()
		return rate
	}
	return 0
}

// estimable is false for metered appliances: their spec only drives the split.
func (r resolvedEnergy) estimable() bool {
	return r.Status == "resolved" && r.DeviceKind == "real_non_metering" && r.Values != nil
}

func nonNegative(v *big.Rat) *big.Rat {
	if v.Sign() < 0 {
		return new(big.Rat)
	}
	return v
}

// wasteKWhPerDay is the record's own waste figure, else its dollar floor
// converted at rate. A v1 record with neither leaves waste unassessed.
func (r resolvedEnergy) wasteKWhPerDay(daily, rate float64) (float64, bool) {
	var floor *float64
	if r.WasteFloor != nil {
		floor = r.WasteFloor.USDPerDay
	}
	return energyrate.ResolutionWasteKWh(daily, rate, r.Values.WasteKWhPerDay, floor)
}

func (r resolvedEnergy) wasteBasis() string {
	if r.Values == nil {
		return ""
	}
	return r.Values.WasteBasis
}
