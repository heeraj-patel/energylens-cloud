package householdenergy

import (
	"math/big"
	"time"
)

// The night window is 23:00-06:00 home-local wall clock (owner decision D-E1).
const (
	nightStartLocal = "23:00"
	nightEndLocal   = "06:00"
)

// nightSpans returns the parts of [from,to) inside the night window, classified
// by local wall clock, so a DST night is one real hour shorter or longer.
func nightSpans(from, to time.Time, loc *time.Location) [][2]time.Time {
	if !to.After(from) {
		return nil
	}
	first := from.In(loc)
	var spans [][2]time.Time
	for n := -1; ; n++ {
		start := time.Date(first.Year(), first.Month(), first.Day()+n, 23, 0, 0, 0, loc)
		if !start.Before(to) {
			return spans
		}
		end := time.Date(first.Year(), first.Month(), first.Day()+n+1, 6, 0, 0, 0, loc)
		if s, e := maxTime(start, from), minTime(end, to); e.After(s) {
			spans = append(spans, [2]time.Time{s, e})
		}
	}
}

// nightFraction is the share of [from,to) inside the night window.
func nightFraction(from, to time.Time, loc *time.Location) *big.Rat {
	if !to.After(from) {
		return new(big.Rat)
	}
	night := int64(0)
	for _, s := range nightSpans(from, to, loc) {
		night += s[1].Sub(s[0]).Nanoseconds()
	}
	return big.NewRat(night, to.Sub(from).Nanoseconds())
}

// nightPriced prices the night part of an interval clipped to [from,to) at the
// interval's own resolved rate.
func nightPriced(item interval, from, to time.Time, tariff marginalTariff, loc *time.Location) *big.Rat {
	total := new(big.Rat)
	for _, s := range nightSpans(maxTime(item.Start, from), minTime(item.End, to), loc) {
		if part := priceIntervalRange(item, s[0], s[1], tariff); part != nil {
			total.Add(total, part)
		}
	}
	return total
}

// nightCoveredShare is the night share of a device's covered seconds in a
// bucket, weighted as floorWasteWh weights coverage.
func nightCoveredShare(energyItems []interval, id string, b bucket, loc *time.Location) *big.Rat {
	covered, night := new(big.Rat), new(big.Rat)
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
		night.Add(night, new(big.Rat).Mul(seconds, nightFraction(start, end, loc)))
	}
	if covered.Sign() == 0 {
		return new(big.Rat)
	}
	return night.Quo(night, covered)
}

// nightComponent returns each metered device's waste cost inside the night
// window, mirroring applyMeasuredSplit's interval and floor classification.
func nightComponent(in componentInput) componentResult {
	res := componentResult{Devices: map[string]deviceAmount{}, Basis: "insufficient_data", Source: "none"}
	if in.Tariff == nil {
		return res
	}
	sums := map[string]*big.Rat{}
	add := func(id string, v *big.Rat) {
		if sums[id] == nil {
			sums[id] = new(big.Rat)
		}
		sums[id].Add(sums[id], v)
	}
	for i, b := range in.Buckets {
		energyItems, costItems := b.deviceItems, b.deviceItems
		if energyItems == nil {
			_, energyItems, _ = selectBucketSources(in.Items, b.Start, b.End)
		}
		if costItems == nil {
			costItems = in.Priced
		}
		for _, item := range costItems {
			spec, ok := in.Specs[item.ApplianceID]
			if !ok || spec.StandbyWatts <= 0 || !terminalDevice(item) {
				continue
			}
			if waste, counted := intervalIsWaste(item, spec.StandbyWatts); counted && waste {
				add(item.ApplianceID, nightPriced(item, b.Start, b.End, *in.Tariff, in.Loc))
			}
		}
		if i >= len(in.Rows) {
			continue
		}
		for _, row := range in.Rows[i] {
			spec, ok := in.Specs[row.ApplianceID]
			if !ok || spec.StandbyWatts > 0 || spec.FloorUSDPerDay <= 0 || row.Allocation.Energy == nil {
				continue
			}
			totalWh := new(big.Rat).Mul(mustDecimal(row.Allocation.Energy.Value), big.NewRat(1000, 1))
			wasteWh := floorWasteWh(spec.FloorUSDPerDay, *in.Tariff, b, energyItems, row.ApplianceID, totalWh)
			piece := interval{Start: b.Start, End: b.End, Wh: wasteWh}
			if cost := priceIntervalRange(piece, b.Start, b.End, *in.Tariff); cost != nil {
				add(row.ApplianceID, new(big.Rat).Mul(cost, nightCoveredShare(energyItems, row.ApplianceID, b, in.Loc)))
			}
		}
	}
	for id, v := range sums {
		res.Devices[id] = deviceAmount{Amount: v, Basis: "measured"}
	}
	return res
}
