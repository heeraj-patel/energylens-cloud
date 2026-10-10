package householdenergy

import (
	"math/big"
	"time"
)

// selectBucketSources makes topology selection once for energy, tariff pricing,
// and device attribution. A root utility meter represents the household; its
// descendants may only appear as terminal device allocations.
func selectBucketSources(items []interval, start, end time.Time) (total, devices []interval, wholeHome bool) {
	var utility, candidates, legacy []interval
	for _, v := range items {
		if !v.Start.Before(end) || !v.End.After(start) {
			continue
		}
		if v.SensorKind == "utility_meter" && v.ParentSensorID == nil {
			utility = append(utility, v)
			continue
		}
		if terminalDevice(v) {
			candidates = append(candidates, v)
		} else if v.SensorKind == "" {
			legacy = append(legacy, v)
		}
	}
	if len(utility) > 0 {
		if sourceConflict(utility) {
			return nil, nil, false
		}
		// Multiple root utility meters cannot establish one home-wide source.
		for i := 1; i < len(utility); i++ {
			if utility[i].SensorID != utility[0].SensorID || utility[i].HubID != utility[0].HubID || utility[i].Epoch != utility[0].Epoch {
				return nil, nil, false
			}
		}
		rootID := utility[0].SensorID
		attached := make([]interval, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.TopologyComplete && candidate.TopologyRootKind != nil && *candidate.TopologyRootKind == "utility_meter" && candidate.TopologyRootID != nil && *candidate.TopologyRootID == rootID {
				attached = append(attached, candidate)
			}
		}
		return utility, excludeConflictingDevices(attached), true
	}
	if len(candidates) > 0 {
		candidates = excludeConflictingDevices(candidates)
		if len(candidates) == 0 {
			return nil, nil, false
		}
		return candidates, candidates, false
	}
	if len(legacy) > 0 {
		if sourceConflict(legacy) {
			return nil, nil, false
		}
		return legacy, nil, false
	}
	// Circuit-only and unattached branches do not establish household energy.
	return nil, nil, false
}

func terminalDevice(v interval) bool {
	if v.ApplianceID == "" || (v.SensorKind != "plug" && v.SensorKind != "strip_outlet") {
		return false
	}
	// Compatibility for isolated calculation tests and older callers that do
	// not carry persisted sensor IDs/topology metadata.
	if v.SensorID == "" {
		return v.ParentSensorID == nil || v.TopologyTerminal
	}
	return v.TopologyComplete && v.TopologyTerminal
}

func excludeConflictingDevices(items []interval) []interval {
	conflicted := make([]bool, len(items))
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if !items[i].Start.Before(items[j].End) || !items[j].Start.Before(items[i].End) {
				continue
			}
			if items[i].SensorID == "" || items[j].SensorID == "" || items[i].SensorID == items[j].SensorID || items[i].ApplianceID == items[j].ApplianceID {
				conflicted[i], conflicted[j] = true, true
			}
		}
	}
	selected := make([]interval, 0, len(items))
	for i, item := range items {
		if !conflicted[i] {
			selected = append(selected, item)
		}
	}
	return selected
}

func sourceConflict(items []interval) bool {
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if items[i].Start.Before(items[j].End) && items[j].Start.Before(items[i].End) {
				return true
			}
		}
	}
	return false
}

func allWholeHomeBuckets(buckets []bucket) bool {
	measured := false
	for _, b := range buckets {
		if b.energyKWh == nil {
			continue
		}
		measured = true
		if !b.wholeHome {
			return false
		}
	}
	return measured
}

func measuredResidual(total allocation, devices []deviceAllocation) *allocation {
	if total.Energy == nil {
		return nil
	}
	energy := mustDecimal(total.Energy.Value)
	for _, d := range devices {
		if d.Allocation.Energy == nil {
			return nil
		}
		energy.Sub(energy, mustDecimal(d.Allocation.Energy.Value))
	}
	if energy.Sign() < 0 {
		return nil
	}
	residual := fromKWh(energy)
	if total.Cost != nil {
		whole := total.Cost.(money)
		cost := mustDecimal(whole.Amount)
		for _, d := range devices {
			if d.Allocation.Cost == nil {
				return nil
			}
			part := d.Allocation.Cost.(money)
			if part.Currency != whole.Currency {
				return nil
			}
			cost.Sub(cost, mustDecimal(part.Amount))
		}
		if cost.Sign() < 0 {
			return nil
		}
		value := money{Amount: cost.FloatString(6), Currency: whole.Currency}
		residual.Cost, residual.UnclassifiedCost, residual.EvidenceBasis = value, value, "mixed"
	}
	return &residual
}

func selectedCoverage(items []interval, start, end time.Time) any {
	if len(items) == 0 {
		return nil
	}
	covered := new(big.Rat)
	for _, v := range items {
		if v.Covered == nil || v.Unavailable || v.Wh == nil {
			return nil
		}
		low, high := maxTime(v.Start, start), minTime(v.End, end)
		if !high.After(low) {
			continue
		}
		share := new(big.Rat).SetFrac(big.NewInt(high.Sub(low).Nanoseconds()), big.NewInt(v.End.Sub(v.Start).Nanoseconds()))
		covered.Add(covered, new(big.Rat).Mul(v.Covered, share))
	}
	duration := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(int64(time.Second)))
	if covered.Cmp(duration) > 0 {
		covered.Set(duration)
	}
	return new(big.Rat).Quo(covered, duration).FloatString(6)
}
