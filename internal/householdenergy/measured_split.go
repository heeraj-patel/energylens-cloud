package householdenergy

import (
	"context"
	"math"
	"math/big"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// standbyThresholdFactor scales a spec's standby watts into the mean-power
// ceiling under which a measured interval counts as standby or idle waste.
const standbyThresholdFactor = 1.5

// readStandbySpecs returns the current datawiz standby watts per appliance.
func readStandbySpecs(ctx context.Context, tx pgx.Tx, home string) (map[string]float64, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text,r.record->'values'->>'standby_watts' FROM appliances a JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||a.id::text AND r.record->'appliance_version'=to_jsonb(a.version) WHERE a.home_id=$1 AND a.archived_at IS NULL AND r.record->'values'->>'standby_watts' IS NOT NULL`, home)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	specs := map[string]float64{}
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if watts, e := strconv.ParseFloat(raw, 64); e == nil && watts > 0 && !math.IsInf(watts, 0) && !math.IsNaN(watts) {
			specs[id] = watts
		}
	}
	return specs, rows.Err()
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

// applyMeasuredSplit divides measured device rows that carry no split into
// useful and waste using the datawiz standby spec; without one the whole total
// is useful. Totals are never changed.
func applyMeasuredSplit(buckets []bucket, rows [][]deviceAllocation, periodRows []deviceAllocation, items, priced []interval, specs map[string]float64, tariff *marginalTariff) {
	periodWasteWh := map[string]*big.Rat{}
	periodWasteCost := map[string]*big.Rat{}
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
			standby := specs[id]
			a := &rows[i][j].Allocation
			if hasSplit(*a) || a.Energy == nil {
				continue
			}
			wasteWh := new(big.Rat)
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
			if periodWasteWh[id] == nil {
				periodWasteWh[id] = new(big.Rat)
			}
			periodWasteWh[id].Add(periodWasteWh[id], wasteWh)
			setSplitEnergy(a, wasteWh)
			cost, isMoney := a.Cost.(money)
			if !isMoney || tariff == nil {
				continue
			}
			wasteCost := new(big.Rat)
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
