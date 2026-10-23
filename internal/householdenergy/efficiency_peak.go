package householdenergy

import (
	"math/big"
	"time"
)

// peakShiftable are the appliance categories whose use can move to a cheaper hour (owner decision D-E4).
var peakShiftable = map[string]bool{
	"dishwasher": true, "washer": true, "clothes_washer": true, "dryer": true,
	"ev_charger": true, "pool_pump": true, "water_heater": true,
}

func hasTOUPeriods(t *marginalTariff) bool {
	for _, s := range t.Segments {
		if len(s.Periods) > 0 && s.Loc != nil {
			return true
		}
	}
	return false
}

// lowestAdder is the smallest adder (0 where no row covers) over the home-local day of at.
func (t *marginalTariff) lowestAdder(at time.Time) *big.Rat {
	local := at.In(t.Loc)
	seconds := []int{0}
	for _, p := range t.Periods {
		seconds = append(seconds, p.Start, p.End)
	}
	var low *big.Rat
	for _, sec := range seconds {
		if sec >= 86400 {
			continue
		}
		adder := new(big.Rat)
		for _, p := range t.Periods {
			if p.covers(local.Month(), local.Weekday(), sec) {
				adder = p.Adder
				break
			}
		}
		if low == nil || adder.Cmp(low) < 0 {
			low = adder
		}
	}
	return low
}

// peakComponent prices each shiftable metered device's energy at its hour's adder
// minus the day's lowest adder: the saving from moving it to the cheapest hour.
func peakComponent(in componentInput) componentResult {
	res := componentResult{Devices: map[string]deviceAmount{}, Basis: "insufficient_data", Source: "none"}
	if in.Tariff == nil {
		return res
	}
	if !hasTOUPeriods(in.Tariff) {
		res.Basis = "flat_tariff"
		return res
	}
	nonElectric := map[string]bool{}
	for _, r := range in.DeviceRows {
		if r.Allocation.Fuel != "" && r.Allocation.Fuel != "electricity" {
			nonElectric[r.ApplianceID] = true
		}
	}
	sums := map[string]*big.Rat{}
	for _, item := range in.Priced {
		id := item.ApplianceID
		if !peakShiftable[in.Specs[id].Category] || !in.Metered[id] || nonElectric[id] ||
			item.Unavailable || !terminalDevice(item) || (item.Fuel != nil && *item.Fuel != "electricity") ||
			!item.End.After(item.Start) {
			continue
		}
		if sums[id] == nil {
			sums[id] = new(big.Rat)
		}
		sums[id].Add(sums[id], peakSaving(item, *in.Tariff))
	}
	for id, v := range sums {
		res.Devices[id] = deviceAmount{Amount: v, Basis: "measured"}
	}
	return res
}

func peakSaving(item interval, schedule marginalTariff) *big.Rat {
	total := new(big.Rat)
	cuts := tariffCuts(item.Start, item.End, schedule.Segments)
	for i := 0; i+1 < len(cuts); i++ {
		t := tariffAt(cuts[i], schedule.Segments)
		if t == nil || len(t.Periods) == 0 || t.Loc == nil {
			continue
		}
		pieces := t.pricedPieces(cuts[i], cuts[i+1])
		if item.Summarized && len(pieces) > 1 {
			continue
		}
		for _, piece := range pieces {
			diff := new(big.Rat).Sub(new(big.Rat).Sub(piece.rate, t.Rate), t.lowestAdder(piece.from))
			if diff.Sign() <= 0 {
				continue
			}
			share := new(big.Rat).SetFrac(big.NewInt(piece.to.Sub(piece.from).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
			kwh := new(big.Rat).Quo(new(big.Rat).Mul(item.Wh, share), big.NewRat(1000, 1))
			total.Add(total, kwh.Mul(kwh, diff))
		}
	}
	return total
}
