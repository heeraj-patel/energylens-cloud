package householdenergy

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"time"

	"energylens.local/cloud/internal/energyrate"
	"github.com/jackc/pgx/v5"
)

// standbyThresholdFactor scales a spec's standby watts into the mean-power
// ceiling under which a measured interval counts as standby or idle waste.
const standbyThresholdFactor = 1.5

// splitSpec is a device's datawiz standby watts and waste floor in dollars
// per day; either may be zero.
type splitSpec struct{ StandbyWatts, FloorUSDPerDay float64 }

// readSplitSpecs returns the current datawiz record's split inputs per appliance.
func readSplitSpecs(ctx context.Context, tx pgx.Tx, home string) (map[string]splitSpec, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text,r.record->'values'->>'standby_watts',r.record->'waste_floor'->>'usd_per_day' FROM appliances a JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||a.id::text AND r.record->'appliance_version'=to_jsonb(a.version) WHERE a.home_id=$1 AND a.archived_at IS NULL`, home)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	specs := map[string]splitSpec{}
	for rows.Next() {
		var id string
		var standby, floor *string
		if err = rows.Scan(&id, &standby, &floor); err != nil {
			return nil, err
		}
		specs[id] = splitSpec{positiveFloat(standby), positiveFloat(floor)}
	}
	return specs, rows.Err()
}

func positiveFloat(raw *string) float64 {
	if raw == nil {
		return 0
	}
	if v, err := strconv.ParseFloat(*raw, 64); err == nil && v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return v
	}
	return 0
}

// intervalIsWaste reports whether an interval's mean power is positive and at
// or below the standby ceiling. Zero-energy intervals belong to neither class.
func intervalIsWaste(item interval, standbyWatts float64) (waste, counted bool) {
	span := item.End.Sub(item.Start).Nanoseconds()
	if span <= 0 || item.Wh == nil || item.Wh.Sign() <= 0 {
		return false, false
	}
	mean := new(big.Rat).Quo(new(big.Rat).Mul(item.Wh, big.NewRat(3_600_000_000_000, 1)), big.NewRat(span, 1))
	ceiling := new(big.Rat).SetFloat64(standbyWatts * standbyThresholdFactor)
	return mean.Cmp(ceiling) <= 0, true
}

func hasSplit(a allocation) bool {
	return a.UsefulEnergy != nil || a.UsefulCost != nil || a.WasteEnergy != nil || a.WasteCost != nil
}

// floorWasteWh is the dollar floor converted at the bucket's rate and
// pro-rated to the hours the device was covered, capped at the row's total.
func floorWasteWh(floorUSDPerDay float64, schedule marginalTariff, b bucket, energyItems []interval, id string, total *big.Rat) *big.Rat {
	covered := new(big.Rat)
	for _, item := range energyItems {
		if item.ApplianceID != id || item.Unavailable || !terminalDevice(item) {
			continue
		}
		start, end := maxTime(item.Start, b.Start), minTime(item.End, b.End)
		if !end.After(start) {
			continue
		}
		share := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
		seconds := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(int64(time.Second)))
		if item.Covered != nil {
			seconds = new(big.Rat).Mul(item.Covered, share)
		}
		covered.Add(covered, seconds)
	}
	if limit := new(big.Rat).SetFrac(big.NewInt(b.End.Sub(b.Start).Nanoseconds()), big.NewInt(int64(time.Second))); covered.Cmp(limit) > 0 {
		covered.Set(limit)
	}
	floorKWh := energyrate.FloorKWh(floorUSDPerDay, bucketRate(schedule, b))
	waste := mustDecimal(strconv.FormatFloat(floorKWh*1000, 'f', 9, 64))
	waste.Mul(waste, covered).Quo(waste, big.NewRat(86400, 1))
	if waste.Cmp(total) > 0 {
		return new(big.Rat).Set(total)
	}
	return waste
}

// applyMeasuredSplit divides measured device rows that carry no split into
// useful and waste: by the datawiz standby spec, else by the converted waste
// floor. A row with no datawiz record stays unsplit. Totals are never changed.
func applyMeasuredSplit(buckets []bucket, rows [][]deviceAllocation, periodRows []deviceAllocation, items, priced []interval, specs map[string]splitSpec, tariff, national *marginalTariff) {
	periodWasteWh := map[string]*big.Rat{}
	periodWasteCost := map[string]*big.Rat{}
	periodMarks := map[string][2]string{}
	cumulative := map[string]*big.Rat{}
	for i := range buckets {
		energyItems := buckets[i].deviceItems
		if energyItems == nil {
			_, energyItems, _ = selectBucketSources(items, buckets[i].Start, buckets[i].End)
		}
		costItems := buckets[i].deviceItems
		if costItems == nil {
			costItems = priced
		}
		for j := range rows[i] {
			id := rows[i][j].ApplianceID
			spec, hasSpec := specs[id]
			standby := spec.StandbyWatts
			a := &rows[i][j].Allocation
			if hasSplit(*a) || a.Energy == nil || !hasSpec {
				continue
			}
			floorSchedule, wasteBasis, rateBasis := tariff, "", ""
			if standby <= 0 && spec.FloorUSDPerDay > 0 {
				wasteBasis, rateBasis = "floor", "home_tariff"
				if floorSchedule == nil {
					floorSchedule, rateBasis = national, "us_average"
				}
				if floorSchedule == nil {
					continue
				}
			} else if standby > 0 {
				wasteBasis = "standby"
			}
			wasteWh := new(big.Rat)
			if wasteBasis == "floor" {
				wasteWh = floorWasteWh(spec.FloorUSDPerDay, *floorSchedule, buckets[i], energyItems, id, new(big.Rat).Mul(mustDecimal(a.Energy.Value), big.NewRat(1000, 1)))
			} else {
				for _, item := range energyItems {
					if item.ApplianceID != id || item.Unavailable || !terminalDevice(item) {
						continue
					}
					if waste, counted := intervalIsWaste(item, standby); counted && waste {
						start, end := maxTime(item.Start, buckets[i].Start), minTime(item.End, buckets[i].End)
						share := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
						wasteWh.Add(wasteWh, new(big.Rat).Mul(item.Wh, share))
					}
				}
			}
			if periodWasteWh[id] == nil {
				periodWasteWh[id] = new(big.Rat)
			}
			periodWasteWh[id].Add(periodWasteWh[id], wasteWh)
			setSplitEnergy(a, wasteWh)
			a.WasteBasis, a.RateBasis = wasteBasis, rateBasis
			periodMarks[id] = [2]string{wasteBasis, rateBasis}
			cost, isMoney := a.Cost.(money)
			if !isMoney || tariff == nil {
				continue
			}
			if rateBasis == "" {
				a.RateBasis, periodMarks[id] = "home_tariff", [2]string{wasteBasis, "home_tariff"}
			}
			wasteCost := new(big.Rat)
			if wasteBasis == "floor" {
				piece := interval{Start: buckets[i].Start, End: buckets[i].End, Wh: wasteWh}
				if part := priceIntervalRange(piece, buckets[i].Start, buckets[i].End, *tariff); part != nil {
					wasteCost.Add(wasteCost, part)
				}
			} else {
				for _, item := range costItems {
					if item.ApplianceID != id || !terminalDevice(item) {
						continue
					}
					if waste, counted := intervalIsWaste(item, standby); counted && waste {
						if part := priceIntervalRange(item, buckets[i].Start, buckets[i].End, *tariff); part != nil {
							wasteCost.Add(wasteCost, part)
						}
					}
				}
			}
			if cumulative[id] == nil {
				cumulative[id], periodWasteCost[id] = new(big.Rat), new(big.Rat)
			}
			cumulative[id].Add(cumulative[id], wasteCost)
			shown := new(big.Rat).Sub(mustDecimal(cumulative[id].FloatString(6)), periodWasteCost[id])
			rowCost := mustDecimal(cost.Amount)
			if shown.Sign() < 0 {
				shown = new(big.Rat)
			} else if shown.Cmp(rowCost) > 0 {
				shown = rowCost
			}
			periodWasteCost[id].Add(periodWasteCost[id], shown)
			setSplitCost(a, cost, shown)
		}
	}
	for i := range periodRows {
		id := periodRows[i].ApplianceID
		a := &periodRows[i].Allocation
		if hasSplit(*a) || a.Energy == nil || periodWasteWh[id] == nil {
			continue
		}
		setSplitEnergy(a, periodWasteWh[id])
		a.WasteBasis, a.RateBasis = periodMarks[id][0], periodMarks[id][1]
		if cost, isMoney := a.Cost.(money); isMoney && periodWasteCost[id] != nil {
			setSplitCost(a, cost, periodWasteCost[id])
		}
	}
}

func setSplitEnergy(a *allocation, wasteWh *big.Rat) {
	waste := mustDecimal(new(big.Rat).Quo(wasteWh, big.NewRat(1000, 1)).FloatString(12))
	useful := new(big.Rat).Sub(mustDecimal(a.Energy.Value), waste)
	a.WasteEnergy = &quantity{Value: waste.FloatString(12), Unit: "kWh"}
	a.UsefulEnergy = &quantity{Value: useful.FloatString(12), Unit: "kWh"}
	a.UnclassifiedEnergy = nil
}

func setSplitCost(a *allocation, total money, waste *big.Rat) {
	useful := new(big.Rat).Sub(mustDecimal(total.Amount), waste)
	a.WasteCost = money{Amount: waste.FloatString(6), Currency: total.Currency}
	a.UsefulCost = money{Amount: useful.FloatString(6), Currency: total.Currency}
	a.UnclassifiedCost = nil
}
