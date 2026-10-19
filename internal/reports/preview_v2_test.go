package reports

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVettedGasEstimateRendersDistinctThermUnit(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	fact, rate := uuid.NewString(), uuid.NewString()
	raw := []byte(fmt.Sprintf(`{"kind":"estimated_useful_cost","energy":{"value":"2.5","unit":"therm","fuel":"natural_gas"},"cost":{"value":"3.00","currency":"USD"},"period":{"start":"2026-10-01","end":"2026-11-01"},"quality":{"source":"reviewed_exact"},"evidence_refs":[{"id":%q,"kind":"reviewed_usage_fact","revision":1},{"id":%q,"kind":"tariff","revision":1}]}`, fact, rate))
	estimate := vettedEstimate(raw, uuid.NewString(), "2026-10-01", "2026-11-01", now)
	if estimate == nil || estimate.EnergyTherms != "2.5" || estimate.EnergyKWh != "" || estimate.Fuel != "natural_gas" {
		t.Fatalf("gas estimate rejected or mislabeled: %+v", estimate)
	}
	p := Preview{ID: uuid.NewString(), HomeID: uuid.NewString(), SchemaVersion: "manual-preview/v2", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Scope: "selected_appliances", Items: []Item{{ApplianceID: uuid.NewString(), Category: "dryer", Estimate: estimate}}, Recommendations: []ReportRecommendation{}}
	var err error
	p.SnapshotSHA256, err = snapshotDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderApprovedHTML(p, p.SnapshotSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.HTML, "2.5 therm natural gas") || strings.Contains(rendered.HTML, "2.5 kWh") {
		t.Fatalf("gas unit missing or confused: %s", rendered.HTML)
	}
	changed := strings.Replace(string(raw), `"natural_gas"`, `"electricity"`, 1)
	if vettedEstimate([]byte(changed), uuid.NewString(), "2026-10-01", "2026-11-01", now) != nil {
		t.Fatal("mismatched gas fuel accepted")
	}
}

func TestVettedDatawizEstimateRequiresCurrentApplianceAndCompatibleEvidence(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appliance, tariff := uuid.NewString(), uuid.NewString()
	raw := fmt.Sprintf(`{"kind":"estimated_operating_cost","formula_version":"datawiz-resolution/v1","energy":{"value":"0.248000","unit":"kWh","fuel":"electricity"},"cost":{"value":"0.049600","currency":"USD"},"period":{"start":"2026-10-01","end":"2026-11-01"},"quality":{"source":"datawiz_resolution","consumption":"resolved"},"evidence_refs":[{"id":%q,"kind":"datawiz_resolution","revision":1},{"id":%q,"kind":"tariff","revision":1}]}`, appliance, tariff)
	if got := vettedEstimate([]byte(raw), appliance, "2026-10-01", "2026-11-01", now); got == nil || got.EnergyKWh != "0.248000" || got.CostUSD != "0.049600" || len(got.SourceRefs) != 2 {
		t.Fatalf("valid Datawiz estimate: %+v", got)
	}
	for name, changed := range map[string]string{
		"foreign appliance": appliance,
		"missing tariff":    strings.Replace(raw, `"kind":"tariff"`, `"kind":"catalog_fact"`, 1),
		"mixed legacy fact": strings.Replace(raw, `"kind":"datawiz_resolution"`, `"kind":"catalog_fact"`, 1),
		"wrong unit":        strings.Replace(raw, `"unit":"kWh"`, `"unit":"therm"`, 1),
		"zero energy":       strings.Replace(raw, `"value":"0.248000"`, `"value":"0"`, 1),
		"malformed cost":    strings.Replace(raw, `"value":"0.049600"`, `"value":"NaN"`, 1),
		"foreign evidence":  strings.Replace(raw, `"id":"`+appliance+`","kind":"datawiz_resolution"`, `"id":"`+uuid.NewString()+`","kind":"datawiz_resolution"`, 1),
		"unavailable":       strings.Replace(raw, `"estimated_operating_cost"`, `"unavailable"`, 1),
		"wrong formula":     strings.Replace(raw, `"datawiz-resolution/v1"`, `"other"`, 1),
		"unresolved":        strings.Replace(raw, `"consumption":"resolved"`, `"consumption":"unavailable"`, 1),
	} {
		id := appliance
		if name == "foreign appliance" {
			id = uuid.NewString()
		} else if changed == appliance {
			changed = raw
		}
		if got := vettedEstimate([]byte(changed), id, "2026-10-01", "2026-11-01", now); got != nil {
			t.Fatalf("%s accepted: %+v", name, got)
		}
	}
}
