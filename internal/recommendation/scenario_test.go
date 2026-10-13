package recommendation

import "testing"

func TestApplyBenefitScenariosRequiresExplicitComparableEvidence(t *testing.T) {
	usage := Evidence{ID: "answer", Kind: "confirmed_answer", Revision: 2}
	tariff := Evidence{ID: "rate", Kind: "tariff", Revision: 3}
	action := Action{ApplianceID: "appliance", RuleID: "dishwasher.cycle_options", State: "active",
		EvidenceRefs: []Evidence{{ID: "base", Kind: "cycle_energy", Revision: 1}, usage, tariff}, usageUnit: "cycles_per_week", usageFrequency: 4}
	scenario := BenefitScenario{ApplianceID: "appliance", RuleID: action.RuleID,
		BaselineEnergy: "1.0", AlternativeEnergy: "0.8", EnergyUnit: "kWh/use",
		UsagePerWeek: 4, RateUSDPerKWh: "0.25",
		BaselineRef:    Evidence{ID: "base", Kind: "reviewed_usage_fact", Revision: 1},
		AlternativeRef: Evidence{ID: "eco", Kind: "reviewed_cycle_energy", Revision: 1},
		UsageRef:       usage, TariffRef: tariff, SourceURL: "https://manufacturer.example/eco-guide"}
	got := ApplyBenefitScenarios([]Action{action}, []BenefitScenario{scenario})
	if got[0].SavingsAmount == nil || *got[0].SavingsAmount != "10.400000" ||
		got[0].SavingsPeriod != "year" || got[0].SavingsCurrency != "USD" ||
		len(got[0].ScenarioRefs) != 4 || got[0].SourceURL != scenario.SourceURL {
		t.Fatalf("supported scenario: %+v", got[0])
	}
	for name, change := range map[string]func(*BenefitScenario){
		"no alternative source": func(s *BenefitScenario) { s.AlternativeRef.ID = "" },
		"no reviewed URL":       func(s *BenefitScenario) { s.SourceURL = "" },
		"insecure URL":          func(s *BenefitScenario) { s.SourceURL = "http://manufacturer.example/eco-guide" },
		"URL with credentials":  func(s *BenefitScenario) { s.SourceURL = "https://user@manufacturer.example/eco-guide" },
		"wrong usage":           func(s *BenefitScenario) { s.UsagePerWeek = 5 },
		"wrong tariff":          func(s *BenefitScenario) { s.TariffRef.ID = "other" },
		"higher alternative":    func(s *BenefitScenario) { s.AlternativeEnergy = "1.1" },
		"incompatible unit":     func(s *BenefitScenario) { s.EnergyUnit = "kWh/hour" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := scenario
			change(&invalid)
			result := ApplyBenefitScenarios([]Action{action}, []BenefitScenario{invalid})
			if result[0].SavingsAmount != nil || result[0].SourceURL != "" {
				t.Fatalf("unsupported benefit became money: %+v", result[0])
			}
		})
	}
	if duplicate := ApplyBenefitScenarios([]Action{action}, []BenefitScenario{scenario, scenario}); duplicate[0].SavingsAmount != nil {
		t.Fatalf("duplicate scenario selected: %+v", duplicate[0])
	}
	if candidate := ApplyBenefitScenarios([]Action{{ApplianceID: action.ApplianceID, RuleID: action.RuleID, State: "candidate", EvidenceRefs: action.EvidenceRefs, usageUnit: action.usageUnit, usageFrequency: action.usageFrequency}}, []BenefitScenario{scenario}); candidate[0].SavingsAmount != nil {
		t.Fatalf("candidate claimed benefit: %+v", candidate[0])
	}
}
