package recommendation

import (
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"fmt"
	"reflect"
	"testing"
)

func researchTip(id, context string) datawizfacts.Fact {
	return datawizfacts.Fact{
		ID: id, Route: "efficiency", SubjectKey: "efficiency:" + context,
		Field: "efficiency_tip", RecordKey: id,
		Value:  json.RawMessage(fmt.Sprintf(`{"context":%q,"applicable_categories":["hvac"],"text":"source guidance"}`, context)),
		Source: json.RawMessage(`{"url":"https://example.com/guidance"}`), CollectedAt: "2026-10-21T00:00:00Z",
	}
}

func assertTipAttachments(t *testing.T, action Action, want []datawizfacts.Fact) {
	t.Helper()
	if len(action.ResearchEvidence) != len(want) {
		t.Fatalf("research evidence: got %+v, want %+v", action.ResearchEvidence, want)
	}
	for i, fact := range want {
		if !reflect.DeepEqual(action.ResearchEvidence[i], fact) {
			t.Fatalf("research evidence %d: got %+v, want %+v", i, action.ResearchEvidence[i], fact)
		}
	}
	var refs []Evidence
	for _, ref := range action.EvidenceRefs {
		if ref.Kind == "datawiz_efficiency_tip" {
			refs = append(refs, ref)
		}
	}
	if len(refs) != len(want) {
		t.Fatalf("tip refs: got %+v, want %d", refs, len(want))
	}
	for i, fact := range want {
		if refs[i] != (Evidence{ID: fact.ID, Kind: "datawiz_efficiency_tip", Revision: 1}) {
			t.Fatalf("tip ref %d: %+v", i, refs[i])
		}
	}
}

func TestResearchTipAttachmentBoundaryAndFiltering(t *testing.T) {
	contexts := []string{"night", "away", "peak_usage", "habits", "appliance_type", "upgrades", "maintenance"}
	valid := make([]datawizfacts.Fact, len(contexts))
	for i, context := range contexts {
		valid[i] = researchTip(fmt.Sprintf("tip-%d", i), context)
	}
	for _, count := range []int{0, 2, 3, 7} {
		t.Run(fmt.Sprintf("count-%d", count), func(t *testing.T) {
			tips := append([]datawizfacts.Fact(nil), valid[:count]...)
			a := Appliance{ID: "hvac-1", Category: "hvac", Fuel: "electricity", Identity: "unresolved", ResearchTips: tips}
			before := append([]datawizfacts.Fact(nil), tips...)
			for _, evaluate := range []func(Appliance) []Action{Evaluate, EvaluateGasEnabled} {
				got := evaluate(a)
				if len(got) != 1 || got[0].RuleID != "general.gather_information" || got[0].State != "candidate" || got[0].NextQuestion != "confirm_model" || got[0].SavingsAmount != nil {
					t.Fatalf("fallback changed: %+v", got)
				}
				want := valid[:min(count, maxResearchTipsPerAppliance)]
				assertTipAttachments(t, got[0], want)
			}
			if !reflect.DeepEqual(tips, before) {
				t.Fatal("input tips changed")
			}
		})
	}
	invalidField := researchTip("other-field", "night")
	invalidField.Field = "other"
	malformed := researchTip("malformed", "night")
	malformed.Value = json.RawMessage(`{`)
	excluded := researchTip("excluded", "night")
	excluded.Value = json.RawMessage(`{"context":"night","applicable_categories":["hvac"],"excluded_categories":["hvac"]}`)
	mismatchedCategory := researchTip("mismatched-category", "night")
	mismatchedCategory.Value = json.RawMessage(`{"context":"night","applicable_categories":["television"]}`)
	mismatchedContext := researchTip("mismatched-context", "unsupported")
	emptyID := researchTip("", "night")
	tips := []datawizfacts.Fact{invalidField, malformed, excluded, mismatchedCategory, mismatchedContext, emptyID, valid[0], valid[0], valid[1], valid[2], valid[3]}
	a := Appliance{ID: "hvac-1", Category: "hvac", Fuel: "electricity", ResearchTips: tips}
	before := append([]datawizfacts.Fact(nil), tips...)
	got := Evaluate(a)
	assertTipAttachments(t, got[0], valid[:3])
	if !reflect.DeepEqual(tips, before) {
		t.Fatal("input tips changed")
	}
}

func TestResearchTipAttachmentPreservesActionEvidence(t *testing.T) {
	tips := []datawizfacts.Fact{researchTip("one", "habits"), researchTip("two", "appliance_type"), researchTip("three", "upgrades"), researchTip("four", "maintenance")}
	a := Appliance{ID: "heat-pump", Category: "heat_pump", Fuel: "electricity", Identity: "confirmed_exact", ResearchTips: tips,
		Facts: []Evidence{{ID: "energy", Kind: "annual_energy", Revision: 4}}, Tariff: &Evidence{ID: "rate", Kind: "tariff", Revision: 5}}
	got := Evaluate(a)
	if len(got) != 1 || got[0].State != "active" || got[0].NextQuestion != "" || got[0].SavingsAmount != nil {
		t.Fatalf("active action changed: %+v", got)
	}
	assertTipAttachments(t, got[0], tips[:3])
	if len(got[0].EvidenceRefs) != 5 || got[0].EvidenceRefs[0] != a.Facts[0] || got[0].EvidenceRefs[1] != *a.Tariff {
		t.Fatalf("ordinary evidence changed: %+v", got[0].EvidenceRefs)
	}
}

func TestResearchTipAttachmentSharesBudgetAcrossActions(t *testing.T) {
	tips := []datawizfacts.Fact{researchTip("one", "habits"), researchTip("two", "habits"), researchTip("three", "habits"), researchTip("four", "habits")}
	a := Appliance{Category: "hvac", ResearchTips: tips}
	actions := []Action{{RuleID: "first", EvidenceRefs: []Evidence{{ID: "ordinary", Kind: "annual_energy", Revision: 1}}}, {RuleID: "second", EvidenceRefs: []Evidence{}}}
	attachResearchTips(a, actions)
	assertTipAttachments(t, actions[0], tips[:3])
	assertTipAttachments(t, actions[1], nil)
	if len(actions[0].EvidenceRefs) != 4 || actions[0].EvidenceRefs[0].ID != "ordinary" {
		t.Fatalf("first action refs changed: %+v", actions[0].EvidenceRefs)
	}
}

func TestRulesUseUniqueRegistryCategoryMapping(t *testing.T) {
	light := Appliance{ID: "light-1", Category: "light", categoryKey: "light", Fuel: "electricity", Identity: "unresolved", ruleCategoryKeys: map[string]string{"lighting": "light"}, ruleKeyCounts: map[string]int{"light": 1}}
	got := Evaluate(light)
	if len(got) != 1 || got[0].RuleID != "lighting.equivalent_bulb" {
		t.Fatalf("canonical light category: %+v", got)
	}
	hvac := Appliance{ID: "hvac-1", Category: "hvac", categoryKey: "hvac", Fuel: "electricity", Identity: "unresolved", ruleCategoryKeys: map[string]string{"heat_pump": "hvac", "air_conditioner": "hvac"}, ruleKeyCounts: map[string]int{"hvac": 2}}
	if got = Evaluate(hvac); len(got) != 1 || got[0].RuleID != "general.gather_information" {
		t.Fatalf("ambiguous HVAC mapping should retain existing behavior: %+v", got)
	}
}

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

func TestGasAppliancesGetNoElectricStandbyOrOperatingRules(t *testing.T) {
	for _, category := range []string{"water_heater", "boiler", "furnace", "space_heater"} {
		for _, fuel := range []string{"natural_gas", "propane"} {
			got := Evaluate(Appliance{Category: category, Fuel: fuel, Identity: "confirmed_exact", Facts: []Evidence{{ID: "f", Kind: "annual_energy", Revision: 1}}, Tariff: &Evidence{ID: "rate", Kind: "tariff", Revision: 1}})
			if len(got) != 1 || got[0].RuleID != "general.gather_information" || got[0].State != "candidate" {
				t.Fatalf("%s/%s got an electric rule: %+v", category, fuel, got)
			}
		}
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
	want := []string{"a.ready", "b.ready", "a.rate", "a.usage", "a.source", "b.review"}
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

func TestRankActionsKeepsEveryActionVisible(t *testing.T) {
	input := []Action{
		{ApplianceID: "a", RuleID: "a.first", State: "active", BenefitGroup: "first"},
		{ApplianceID: "a", RuleID: "a.second", State: "active", BenefitGroup: "second"},
		{ApplianceID: "a", RuleID: "a.third", State: "active", BenefitGroup: "third"},
		{ApplianceID: "a", RuleID: "a.fourth", State: "active", BenefitGroup: "fourth"},
		{ApplianceID: "b", RuleID: "b.first", State: "active", BenefitGroup: "first"},
	}
	got := rankActions(input)
	want := []string{"a.first", "a.fourth", "a.second", "a.third", "b.first"}
	ids := make([]string, 0, len(got))
	for _, action := range got {
		ids = append(ids, action.RuleID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ranked IDs %v; want %v", ids, want)
	}
}

func TestRankActionsOnlyComparesMatchingSuppliedBenefits(t *testing.T) {
	amount := func(value string) *string { return &value }
	ref := []Evidence{{ID: "scenario", Kind: "baseline_alternative", Revision: 2}}
	input := []Action{
		{RuleID: "unknown-first", ApplianceID: "a", State: "active"},
		{RuleID: "low", ApplianceID: "a", State: "active", SavingsAmount: amount("4.00"), SavingsPeriod: "month", SavingsCurrency: "USD", SavingsBasis: "bill_cost", ScenarioRefs: ref},
		{RuleID: "other-period", ApplianceID: "a", State: "active", SavingsAmount: amount("90"), SavingsPeriod: "year", SavingsCurrency: "USD", SavingsBasis: "bill_cost", ScenarioRefs: ref},
		{RuleID: "high", ApplianceID: "b", State: "active", SavingsAmount: amount("12.50"), SavingsPeriod: "month", SavingsCurrency: "USD", SavingsBasis: "bill_cost", ScenarioRefs: ref},
		{RuleID: "unknown-last", ApplianceID: "b", State: "active"},
	}
	got := rankActions(input)
	ids := make([]string, len(got))
	for i, action := range got {
		ids[i] = action.RuleID
	}
	want := []string{"high", "other-period", "unknown-first", "low", "unknown-last"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ranked IDs %v; want %v", ids, want)
	}
	if got[1].ScenarioRefs[0].ID != "scenario" || got[1].SavingsAmount == nil {
		t.Fatalf("scenario provenance lost: %+v", got[1])
	}
	input[3].ScenarioRefs = nil
	got = rankActions(input)
	if got[0].RuleID != "low" || got[3].RuleID != "high" {
		t.Fatalf("benefit without provenance was compared: %+v", got)
	}
}
