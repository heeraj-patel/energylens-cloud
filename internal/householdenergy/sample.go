package householdenergy

import "fmt"

// SampleRevision identifies the coherent internal fixture's flat $0.20/kWh
// cost basis. Sample responses intentionally leave useful/waste unknown.
const SampleRevision = "synthetic_household_v1"

// SampleProvider is installed only by explicit dev/test configuration. Route
// authentication and database ownership checks still run before selection.
type SampleProvider struct {
	AllowedUsers map[string]bool
	AllowedHomes map[string]bool
}

func sampleBuckets(p period, granularity string) []bucket {
	out := makeBuckets(p, granularity, nil)
	for i := range out {
		out[i].Allocation = sampleAllocation(1)
	}
	return out
}

func sampleTotal(count int) allocation { return sampleAllocation(count) }

func sampleAllocation(kwh int) allocation {
	energy := &quantity{Value: fmt.Sprint(kwh), Unit: "kWh"}
	cents := kwh * 20
	cost := struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{Amount: fmt.Sprintf("%d.%02d", cents/100, cents%100), Currency: "USD"}
	return allocation{
		Energy: energy, Cost: cost,
		UnclassifiedEnergy: energy, UnclassifiedCost: cost,
		Availability: "partial", EvidenceBasis: "synthetic",
	}
}
