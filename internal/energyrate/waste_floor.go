package energyrate

import "math"

// FloorKWh converts a waste floor in dollars per day to kWh per day at a rate.
func FloorKWh(usdPerDay, usdPerKWh float64) float64 {
	if usdPerDay <= 0 || usdPerKWh <= 0 {
		return 0
	}
	return usdPerDay / usdPerKWh
}

// WasteFloorKWh is the floor in kWh, never above the day's usage.
func WasteFloorKWh(usdPerDay, usdPerKWh, kwhPerDay float64) float64 {
	return math.Min(FloorKWh(usdPerDay, usdPerKWh), math.Max(kwhPerDay, 0))
}
