package recommendation

import (
	"reflect"
	"testing"
)

func TestEvaluateRequiresExactSourceAndCurrentAnswer(t *testing.T) {
	a := Appliance{ID: "washer-1", Category: "washer", Fuel: "electricity", Identity: "unresolved"}
	got := Evaluate(a)
	if len(got) != 1 || got[0].State != "candidate" || got[0].NextQuestion != "confirm_model" || got[0].SavingsAmount != nil {
		t.Fatalf("unresolved: %+v", got)
	}
	a.Identity = "confirmed_exact"
	a.Facts = []Evidence{{ID: "annual", Kind: "annual_energy", Revision: 1}}
	got = Evaluate(a)
	if got[0].NextQuestion != "find_reviewed_energy_source" {
		t.Fatalf("annual figure used for cycle rule: %+v", got)
	}
	a.Facts = append(a.Facts, Evidence{ID: "cycle", Kind: "cycle_energy", Revision: 2})
	got = Evaluate(a)
	if got[0].NextQuestion != "cycles_per_week" || len(got[0].EvidenceRefs) != 1 {
		t.Fatalf("missing usage: %+v", got)
	}
	a.Answers = map[string]Evidence{"cycles_per_week": {ID: "answer", Kind: "confirmed_answer", Revision: 3}}
	got = Evaluate(a)
	if got[0].State != "candidate" || got[0].NextQuestion != "add_electricity_rate" || len(got[0].EvidenceRefs) != 2 {
		t.Fatalf("missing rate: %+v", got)
	}
	a.Tariff = &Evidence{ID: "rate", Kind: "tariff", Revision: 4}
	got = Evaluate(a)
	if got[0].State != "active" || got[0].NextQuestion != "" || len(got[0].EvidenceRefs) != 3 || got[0].SavingsAmount != nil {
		t.Fatalf("supported action: %+v", got)
	}
}

func TestEvaluateCategoryAndFuelBoundary(t *testing.T) {
	if got := Evaluate(Appliance{Category: "dryer", Fuel: "gas", Identity: "confirmed_exact"}); len(got) != 1 || got[0].State != "candidate" || got[0].RuleID != "general.gather_information" {
		t.Fatalf("gas dryer got electric rule: %+v", got)
	}
	if got := Evaluate(Appliance{Category: "unknown", Fuel: "electricity"}); len(got) != 1 || got[0].State != "candidate" {
		t.Fatalf("unsupported device got a rule: %+v", got)
	}
	a := Appliance{Category: "refrigerator", Fuel: "electricity", Identity: "confirmed_exact", Facts: []Evidence{{ID: "reviewed", Kind: "annual_energy", Revision: 1}}, Tariff: &Evidence{ID: "rate", Kind: "tariff", Revision: 1}}
	got := Evaluate(a)
	if len(got) != 1 || got[0].State != "active" || got[0].SavingsAmount != nil {
		t.Fatalf("refrigerator action: %+v", got)
	}
}

func TestTipCategoryIsExplicitPerSourceRule(t *testing.T) {
	for _, r := range initial {
		want := TipGeneral
		switch r.id {
		case "refrigerator.maintenance":
			want = TipCare
		case "lighting.equivalent_bulb":
			want = TipUpgrade
		}
		got := Evaluate(Appliance{ID: "device", Category: r.category, Fuel: r.fuel})
		if len(got) != 1 || got[0].TipCategory != want {
			t.Fatalf("rule %s category: %+v; want %s", r.id, got, want)
		}
	}
	for _, action := range []Action{
		Evaluate(Appliance{Category: "unknown", Fuel: "electricity"})[0],
		EvaluateGasEnabled(Appliance{Category: "dryer", Fuel: "natural_gas"})[0],
	} {
		if action.TipCategory != TipGeneral {
			t.Fatalf("fallback or gas rule category: %+v", action)
		}
	}
}

func TestGasDryerRequiresOptInAndSeparateEvidence(t *testing.T) {
	a := Appliance{ID: "dryer-1", Category: "dryer", Fuel: "natural_gas", Identity: "confirmed_exact",
		Tariff: &Evidence{ID: "electric-rate", Kind: "tariff", Revision: 1},
		Facts:  []Evidence{{ID: "electric-fact", Kind: "cycle_energy", Revision: 1}}}
	if legacy := Evaluate(a); len(legacy) != 1 || legacy[0].RuleID != "general.gather_information" {
		t.Fatalf("legacy client saw gas rule: %+v", legacy)
	}
	check := func(state, next string, refs int) {
		t.Helper()
		got := EvaluateGasEnabled(a)
		if len(got) != 1 || got[0].RuleID != "dryer.gas_cycle_options" || got[0].State != state ||
			got[0].NextQuestion != next || len(got[0].EvidenceRefs) != refs || got[0].SavingsAmount != nil {
			t.Fatalf("gas action: %+v", got)
		}
	}
	check("candidate", "find_reviewed_energy_source", 0)
	a.GasUsage = &Evidence{ID: "gas-fact", Kind: "gas_cycle_energy", Revision: 1}
	check("candidate", "cycles_per_week", 1)
	a.Answers = map[string]Evidence{"cycles_per_week": {ID: "answer", Kind: "confirmed_answer", Revision: 1}}
	check("candidate", "add_gas_rate", 2)
	a.GasTariff = &Evidence{ID: "gas-rate", Kind: "gas_tariff", Revision: 1}
	check("active", "", 3)
	a.Identity = "unresolved"
	check("candidate", "confirm_model", 0)
	a.Identity = "confirmed_exact"
	a.Fuel = "propane"
	if got := EvaluateGasEnabled(a); len(got) != 1 || got[0].RuleID != "general.gather_information" {
		t.Fatalf("propane received natural-gas rule: %+v", got)
	}
}

func TestExpandedDeviceRulesRequireExactReviewedEvidence(t *testing.T) {
	cases := []struct {
		category, ruleID, factKind, group, answerKind string
	}{
		{"dishwasher", "dishwasher.cycle_options", "cycle_energy", "dishwasher_cycle", "cycles_per_week"},
		{"heat_pump", "heat_pump.operating_guidance", "annual_energy", "hvac_operation", ""},
		{"water_heater", "water_heater.operating_mode", "annual_energy", "water_heating", ""},
		{"dehumidifier", "dehumidifier.humidity_setting", "annual_energy", "humidity_control", ""},
		{"lighting", "lighting.equivalent_bulb", "annual_energy", "lighting_service", ""},
		{"fan", "fan.speed_setting", "annual_energy", "fan_service", ""},
		{"pool_pump", "pool_pump.operating_guidance", "annual_energy", "pool_service", ""},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			a := Appliance{ID: "device", Category: tc.category, Fuel: "electricity", Identity: "documented_variant_family"}
			assert := func(state, next string, refs int) {
				t.Helper()
				got := Evaluate(a)
				if len(got) != 1 || got[0].RuleID != tc.ruleID || got[0].BenefitGroup != tc.group ||
					got[0].State != state || got[0].NextQuestion != next || len(got[0].EvidenceRefs) != refs ||
					got[0].SavingsAmount != nil {
					t.Fatalf("unexpected %s action: %+v", tc.category, got)
				}
			}
			a.Facts = []Evidence{{ID: "reviewed", Kind: tc.factKind, Revision: 1}}
			a.Tariff = &Evidence{ID: "rate", Kind: "tariff", Revision: 1}
			assert("candidate", "confirm_model", 0)
			a.Identity = "confirmed_exact"
			a.Facts = []Evidence{{ID: "wrong", Kind: "standby_power", Revision: 1}}
			assert("candidate", "find_reviewed_energy_source", 0)
			a.Facts = []Evidence{{ID: "reviewed", Kind: tc.factKind, Revision: 1}}
			if tc.answerKind != "" {
				assert("candidate", tc.answerKind, 1)
				a.Answers = map[string]Evidence{tc.answerKind: {ID: "answer", Kind: "confirmed_answer", Revision: 1}}
			}
			a.Tariff = nil
			wantRefs := 1
			if tc.answerKind != "" {
				wantRefs = 2
			}
			assert("candidate", "add_electricity_rate", wantRefs)
			a.Tariff = &Evidence{ID: "rate", Kind: "tariff", Revision: 1}
			assert("active", "", wantRefs+1)
			a.Fuel = "gas"
			got := Evaluate(a)
			if len(got) != 1 || got[0].RuleID != "general.gather_information" || got[0].SavingsAmount != nil {
				t.Fatalf("electric rule applied to gas device: %+v", got)
			}
		})
	}
}

func TestRankActionsEvidenceReadinessAndApplianceScopedExclusivity(t *testing.T) {
	input := []Action{
		{ApplianceID: "b", RuleID: "b.review", State: "candidate", NextQuestion: "confirm_model", BenefitGroup: "annual_energy"},
		{ApplianceID: "a", RuleID: "a.usage", State: "candidate", NextQuestion: "hours_per_week", BenefitGroup: "idle"},
		{ApplianceID: "a", RuleID: "a.rate", State: "candidate", NextQuestion: "add_electricity_rate", BenefitGroup: "idle"},
		{ApplianceID: "b", RuleID: "b.ready", State: "active", BenefitGroup: "annual_energy"},
		{ApplianceID: "a", RuleID: "a.ready", State: "active", BenefitGroup: "annual_energy"},
		{ApplianceID: "a", RuleID: "a.source", State: "candidate", NextQuestion: "find_reviewed_energy_source", BenefitGroup: "other"},
	}
	want := []string{"a.ready", "b.ready", "a.rate", "a.source"}
	assert := func(actions []Action) {
		t.Helper()
		got := rankActions(actions)
		ids := make([]string, 0, len(got))
		for _, action := range got {
			ids = append(ids, action.RuleID)
			if action.SavingsAmount != nil {
				t.Fatalf("ranked action claimed savings: %+v", action)
			}
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("ranked IDs %v; want %v", ids, want)
		}
	}
	assert(input)
	for i, j := 0, len(input)-1; i < j; i, j = i+1, j-1 {
		input[i], input[j] = input[j], input[i]
	}
	assert(input)
}

func TestRankActionsBoundsOneApplianceWithoutDroppingAnother(t *testing.T) {
	input := []Action{
		{ApplianceID: "a", RuleID: "a.first", State: "active", BenefitGroup: "first"},
		{ApplianceID: "a", RuleID: "a.second", State: "active", BenefitGroup: "second"},
		{ApplianceID: "a", RuleID: "a.third", State: "active", BenefitGroup: "third"},
		{ApplianceID: "a", RuleID: "a.fourth", State: "active", BenefitGroup: "fourth"},
		{ApplianceID: "b", RuleID: "b.first", State: "active", BenefitGroup: "first"},
	}
	got := rankActions(input)
	want := []string{"a.first", "a.fourth", "a.second", "b.first"}
	ids := make([]string, 0, len(got))
	for _, action := range got {
		ids = append(ids, action.RuleID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ranked IDs %v; want %v", ids, want)
	}
}
