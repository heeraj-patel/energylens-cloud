package samplehousehold

import (
	"encoding/json"
	"testing"
)

func TestSampleRelationships(t *testing.T) {
	if err := ValidateRelationships(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(map[string]any){
		"foreign appliance home": func(f map[string]any) {
			appliances(f)[0]["home_id"] = "00000000-0000-4000-8000-000000000099"
		},
		"duplicate appliance ID": func(f map[string]any) {
			appliances(f)[1]["id"] = appliances(f)[0]["id"]
		},
		"dangling attachment appliance": func(f map[string]any) {
			attachment(f)["appliance_id"] = "10000000-0000-4000-8000-000000000099"
		},
		"foreign attachment home": func(f map[string]any) {
			attachment(f)["home_id"] = "00000000-0000-4000-8000-000000000099"
		},
		"unmatched sensor": func(f map[string]any) {
			attachment(f)["sensor_id"] = "30000000-0000-4000-8000-000000000099"
		},
		"unmatched ownership epoch": func(f map[string]any) {
			attachment(f)["ownership_epoch"] = float64(2)
		},
		"fabricated energy": func(f map[string]any) {
			summary(f)["energy_wh"] = float64(100)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var fixture map[string]any
			if err := json.Unmarshal(Fixture, &fixture); err != nil {
				t.Fatal(err)
			}
			mutate(fixture)
			data, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRelationships(data); err == nil {
				t.Fatal("accepted incoherent sample fixture")
			}
		})
	}
}

func appliances(f map[string]any) []map[string]any {
	rows := f["domain_api_responses"].(map[string]any)["appliances"].(map[string]any)["appliances"].([]any)
	return []map[string]any{rows[0].(map[string]any), rows[1].(map[string]any)}
}

func attachment(f map[string]any) map[string]any {
	return f["domain_api_relationships"].(map[string]any)["attachments"].([]any)[0].(map[string]any)
}

func summary(f map[string]any) map[string]any {
	return f["domain_api_responses"].(map[string]any)["metered_summaries"].(map[string]any)["summaries"].([]any)[0].(map[string]any)
}
