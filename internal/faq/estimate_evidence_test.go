package faq

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
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

func TestOperatingCostAcceptsOnlyScopedDatawizResolution(t *testing.T) {
	appliance, tariff := uuid.NewString(), uuid.NewString()
	raw := fmt.Sprintf(`{"appliance_id":%q,"kind":"estimated_operating_cost","formula_version":"datawiz-resolution/v1","energy":{"value":"0.248000","unit":"kWh","fuel":"electricity"},"period":{"start":"2026-10-01","end":"2026-11-01"},"cost":{"value":"0.049600","currency":"USD"},"quality":{"source":"datawiz_resolution","consumption":"resolved"},"evidence_refs":[{"id":%q,"kind":"datawiz_resolution","revision":1},{"id":%q,"kind":"tariff","revision":1}]}`, appliance, appliance, tariff)
	if got, ok := OperatingCostFromEstimate(json.RawMessage(raw), appliance); !ok || got.Amount != "0.049600" || len(got.Evidence) != 2 {
		t.Fatalf("valid Datawiz cost: %+v %v", got, ok)
	}
	national := strings.Replace(raw, `"kind":"tariff"`, `"kind":"national_rate"`, 1)
	if got, ok := OperatingCostFromEstimate(json.RawMessage(national), appliance); !ok || len(got.Evidence) != 2 {
		t.Fatalf("national_rate Datawiz cost dropped: %+v %v", got, ok)
	}
	for name, changed := range map[string]string{
		"national rate with extra ref": strings.Replace(national, `"revision":1}]`, `"revision":1},{"id":"`+uuid.NewString()+`","kind":"national_rate","revision":1}]`, 1),
		"foreign resolution":           strings.Replace(raw, `"id":"`+appliance+`","kind":"datawiz_resolution"`, `"id":"`+uuid.NewString()+`","kind":"datawiz_resolution"`, 1),
		"missing tariff":               strings.Replace(raw, `"kind":"tariff"`, `"kind":"catalog_fact"`, 1),
		"mixed legacy fact":            strings.Replace(raw, `"kind":"datawiz_resolution"`, `"kind":"catalog_fact"`, 1),
		"wrong unit":                   strings.Replace(raw, `"unit":"kWh"`, `"unit":"therm"`, 1),
		"zero energy":                  strings.Replace(raw, `"value":"0.248000"`, `"value":"0"`, 1),
		"malformed cost":               strings.Replace(raw, `"value":"0.049600"`, `"value":"NaN"`, 1),
		"unavailable":                  strings.Replace(raw, `"estimated_operating_cost"`, `"unavailable"`, 1),
		"wrong formula":                strings.Replace(raw, `"datawiz-resolution/v1"`, `"other"`, 1),
		"unresolved":                   strings.Replace(raw, `"consumption":"resolved"`, `"consumption":"unavailable"`, 1),
	} {
		if got, ok := OperatingCostFromEstimate(json.RawMessage(changed), appliance); ok {
			t.Fatalf("%s accepted: %+v", name, got)
		}
	}
}
