package faq

import (
	"encoding/json"
	"testing"
)

func TestOperatingCostEstimateRequiresCompatibleScopedEvidence(t *testing.T) {
	appliance := "375aac5e-6a54-4e54-a93a-0c8401acecd8"
	good := json.RawMessage(`{"appliance_id":"375aac5e-6a54-4e54-a93a-0c8401acecd8","kind":"estimated_operating_cost","period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"0.00","currency":"USD"},"evidence_refs":[{"id":"3488226d-a9b2-4266-a66b-6e1121d366b9","kind":"catalog_fact","revision":2},{"id":"4bbb3d63-b604-41b1-b537-c10115c8a585","kind":"tariff","revision":1}]}`)
	got, ok := OperatingCostFromEstimate(good, appliance)
	if !ok || got.Amount != "0.00" || len(got.Evidence) != 2 {
		t.Fatalf("valid zero estimate: %+v %v", got, ok)
	}
	for name, raw := range map[string]json.RawMessage{
		"other appliance": good,
		"useful label":    json.RawMessage(`{"appliance_id":"375aac5e-6a54-4e54-a93a-0c8401acecd8","kind":"estimated_useful_cost","period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"1","currency":"USD"},"evidence_refs":[]}`),
		"missing rate":    json.RawMessage(`{"appliance_id":"375aac5e-6a54-4e54-a93a-0c8401acecd8","kind":"estimated_operating_cost","period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"1","currency":"USD"},"evidence_refs":[{"id":"3488226d-a9b2-4266-a66b-6e1121d366b9","kind":"catalog_fact","revision":2}]}`),
		"negative":        json.RawMessage(`{"appliance_id":"375aac5e-6a54-4e54-a93a-0c8401acecd8","kind":"estimated_operating_cost","period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"-1","currency":"USD"},"evidence_refs":[{"id":"3488226d-a9b2-4266-a66b-6e1121d366b9","kind":"catalog_fact","revision":2},{"id":"4bbb3d63-b604-41b1-b537-c10115c8a585","kind":"tariff","revision":1}]}`),
	} {
		requested := appliance
		if name == "other appliance" {
			requested = "2a60b4ad-6644-4cb4-82bb-bc29011be8af"
		}
		if got, ok := OperatingCostFromEstimate(raw, requested); ok {
			t.Fatalf("%s escaped: %+v", name, got)
		}
	}
}
