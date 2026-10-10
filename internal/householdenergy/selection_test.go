package householdenergy

import (
	"math/big"
	"testing"
	"time"
)

func TestUtilityRootAttributesOnlyAttachedTerminalApplianceMeters(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	root, circuit, outlet, intermediate, independent := "utility", "circuit", "outlet", "intermediate", "independent"
	utilityKind := "utility_meter"
	plugKind := "plug"
	items := []interval{
		{Start: start, End: end, Wh: big.NewRat(1200, 1), SensorID: root, SensorKind: "utility_meter", TopologyComplete: true, TopologyTerminal: false, TopologyRootID: &root, TopologyRootKind: &utilityKind},
		{Start: start, End: end, Wh: big.NewRat(900, 1), SensorID: circuit, SensorKind: "circuit", ParentSensorID: &root, TopologyComplete: true, TopologyRootID: &root, TopologyRootKind: &utilityKind},
		{Start: start, End: end, Wh: big.NewRat(400, 1), SensorID: outlet, SensorKind: "plug", ParentSensorID: &circuit, ApplianceID: "appliance-a", TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &root, TopologyRootKind: &utilityKind},
		{Start: start, End: end, Wh: big.NewRat(500, 1), SensorID: intermediate, SensorKind: "strip_outlet", ParentSensorID: &circuit, ApplianceID: "appliance-b", TopologyComplete: true, TopologyTerminal: false, TopologyRootID: &root, TopologyRootKind: &utilityKind},
		{Start: start, End: end, Wh: big.NewRat(700, 1), SensorID: independent, SensorKind: "plug", ApplianceID: "appliance-c", TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &independent, TopologyRootKind: &plugKind},
	}
	buckets := makeBuckets(period{start, end}, "hour", items)
	if len(buckets) != 1 || buckets[0].energyKWh == nil || buckets[0].energyKWh.Cmp(big.NewRat(6, 5)) != 0 || !buckets[0].wholeHome {
		t.Fatalf("utility source was not kept as sole home total: %+v", buckets)
	}
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].ApplianceID != "appliance-a" || wh["appliance-a"].Cmp(big.NewRat(400, 1)) != 0 {
		t.Fatalf("only attached terminal appliance should be attributed: rows=%+v wh=%v", rows, wh)
	}
	residual := measuredResidual(buckets[0].Allocation, rows[0])
	if residual == nil || residual.Energy == nil || residual.Energy.Value != "0.800000000000" {
		t.Fatalf("home/device residual did not conserve energy: %+v", residual)
	}
}

func TestIndependentTerminalDescendantsFormTotalWithoutDoubleCountingAncestors(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	strip, circuit, first, second, unsupported := "strip", "circuit", "outlet-a", "outlet-b", "circuit-only"
	circuitKind := "circuit"
	items := []interval{
		{Start: start, End: end, Wh: big.NewRat(900, 1), SensorID: strip, SensorKind: "circuit", TopologyComplete: true, TopologyTerminal: false, TopologyRootID: &strip, TopologyRootKind: &circuitKind},
		{Start: start, End: end, Wh: big.NewRat(600, 1), SensorID: circuit, SensorKind: "circuit", ParentSensorID: &strip, TopologyComplete: true, TopologyTerminal: false, TopologyRootID: &strip, TopologyRootKind: &circuitKind},
		{Start: start, End: end, Wh: big.NewRat(100, 1), SensorID: first, SensorKind: "strip_outlet", ParentSensorID: &circuit, ApplianceID: "appliance-a", TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &strip, TopologyRootKind: &circuitKind},
		{Start: start, End: end, Wh: big.NewRat(200, 1), SensorID: second, SensorKind: "plug", ParentSensorID: &strip, ApplianceID: "appliance-b", TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &strip, TopologyRootKind: &circuitKind},
		{Start: start, End: end, Wh: big.NewRat(300, 1), SensorID: unsupported, SensorKind: "circuit", ParentSensorID: &strip, TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &strip, TopologyRootKind: &circuitKind},
	}
	buckets := makeBuckets(period{start, end}, "hour", items)
	if len(buckets) != 1 || buckets[0].energyKWh == nil || buckets[0].energyKWh.Cmp(big.NewRat(3, 10)) != 0 || buckets[0].wholeHome {
		t.Fatalf("terminal outlet total included an ancestor or unsupported branch: %+v", buckets)
	}
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	if len(rows) != 1 || len(rows[0]) != 2 || wh["appliance-a"].Cmp(big.NewRat(100, 1)) != 0 || wh["appliance-b"].Cmp(big.NewRat(200, 1)) != 0 {
		t.Fatalf("independent terminal appliances were not retained: rows=%+v wh=%v", rows, wh)
	}
}

func TestAmbiguousDeviceBranchDoesNotRemoveIndependentTerminalBranch(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	root, duplicateA, duplicateB, independent := "circuit", "plug-a", "plug-b", "plug-c"
	kind := "circuit"
	mk := func(sensor, appliance string, wh int64) interval {
		return interval{Start: start, End: end, Wh: big.NewRat(wh, 1), SensorID: sensor, SensorKind: "plug", ParentSensorID: &root, ApplianceID: appliance, TopologyComplete: true, TopologyTerminal: true, TopologyRootID: &root, TopologyRootKind: &kind}
	}
	items := []interval{
		mk(duplicateA, "duplicate-appliance", 100),
		mk(duplicateB, "duplicate-appliance", 200),
		mk(independent, "independent-appliance", 300),
	}
	buckets := makeBuckets(period{start, end}, "hour", items)
	if len(buckets) != 1 || buckets[0].energyKWh == nil || buckets[0].energyKWh.Cmp(big.NewRat(3, 10)) != 0 {
		t.Fatalf("ambiguous branch hid independent branch or double counted: %+v", buckets)
	}
	rows, wh := attributeDeviceBuckets(buckets, items, nil)
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].ApplianceID != "independent-appliance" || wh["independent-appliance"].Cmp(big.NewRat(300, 1)) != 0 {
		t.Fatalf("only the independent branch should remain: rows=%+v wh=%v", rows, wh)
	}
}

func TestCircuitOnlyTopologyDoesNotEstablishHomeEnergy(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	items := []interval{{Start: start, End: start.Add(time.Hour), Wh: big.NewRat(900, 1), SensorID: "circuit", SensorKind: "circuit"}}
	selected, devices, wholeHome := selectBucketSources(items, start, start.Add(time.Hour))
	if len(selected) != 0 || len(devices) != 0 || wholeHome {
		t.Fatalf("circuit-only energy should remain unclassified: selected=%+v devices=%+v whole=%v", selected, devices, wholeHome)
	}
}
