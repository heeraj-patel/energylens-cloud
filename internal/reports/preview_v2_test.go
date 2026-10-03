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
	estimate := vettedEstimate(raw, "2026-10-01", "2026-11-01", now)
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
	if vettedEstimate([]byte(changed), "2026-10-01", "2026-11-01", now) != nil {
		t.Fatal("mismatched gas fuel accepted")
	}
}
