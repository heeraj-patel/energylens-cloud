package recommendation

import (
	"math/big"
	"net/url"
	"strings"
)

// BenefitScenario is an explicit, action-specific comparison. Neither an
// annual rating nor a tariff alone supplies the alternative in this input.
// The caller must resolve all four current evidence revisions in one snapshot.
type BenefitScenario struct {
	ApplianceID       string
	RuleID            string
	BaselineEnergy    string // kWh per use or kWh per hour
	AlternativeEnergy string // same unit and service as baseline
	EnergyUnit        string // kWh/use or kWh/hour
	UsagePerWeek      int64
	RateUSDPerKWh     string
	BaselineRef       Evidence
	AlternativeRef    Evidence
	UsageRef          Evidence
	TariffRef         Evidence
	SourceURL         string // reviewed alternative source, never a candidate link
}

// ApplyBenefitScenarios populates comparable annual USD benefits only when an
// active action and a complete explicit scenario agree on appliance, rule,
// usage, tariff and period. Unknown scenarios leave the card intact.
func ApplyBenefitScenarios(actions []Action, scenarios []BenefitScenario) []Action {
	out := append([]Action(nil), actions...)
	byAction := make(map[string][]BenefitScenario, len(scenarios))
	for _, scenario := range scenarios {
		key := scenario.ApplianceID + "\x00" + scenario.RuleID
		byAction[key] = append(byAction[key], scenario)
	}
	for i := range out {
		action := &out[i]
		key := action.ApplianceID + "\x00" + action.RuleID
		matches := byAction[key]
		if action.State != "active" || len(matches) != 1 {
			continue
		}
		scenario := matches[0]
		amount, ok := scenarioBenefit(*action, scenario)
		if !ok {
			continue
		}
		action.SavingsAmount = &amount
		action.SavingsPeriod = "year"
		action.SavingsCurrency = "USD"
		action.SavingsBasis = "reviewed_action_scenario"
		action.ScenarioRefs = []Evidence{scenario.BaselineRef, scenario.AlternativeRef, scenario.UsageRef, scenario.TariffRef}
		action.SourceURL = scenario.SourceURL
	}
	return out
}

func scenarioBenefit(action Action, scenario BenefitScenario) (string, bool) {
	source, sourceErr := url.Parse(scenario.SourceURL)
	if sourceErr != nil || source.Scheme != "https" || source.Host == "" || source.User != nil || source.Fragment != "" {
		return "", false
	}
	if scenario.ApplianceID == "" || scenario.RuleID == "" || scenario.UsagePerWeek < 0 ||
		(action.usageUnit == "hours_per_week" && (scenario.EnergyUnit != "kWh/hour" || scenario.UsagePerWeek > 168)) ||
		(action.usageUnit == "cycles_per_week" && (scenario.EnergyUnit != "kWh/use" || scenario.UsagePerWeek > 100)) ||
		(action.usageUnit != "hours_per_week" && action.usageUnit != "cycles_per_week") ||
		action.usageFrequency != scenario.UsagePerWeek {
		return "", false
	}
	refs := []Evidence{scenario.BaselineRef, scenario.AlternativeRef, scenario.UsageRef, scenario.TariffRef}
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref.ID == "" || ref.Kind == "" || ref.Revision <= 0 || seen[ref.ID] {
			return "", false
		}
		seen[ref.ID] = true
	}
	if scenario.UsageRef.Kind != "confirmed_answer" || scenario.TariffRef.Kind != "tariff" ||
		!hasEvidence(action.EvidenceRefs, scenario.UsageRef) || !hasEvidence(action.EvidenceRefs, scenario.TariffRef) ||
		!hasCurrentBaseline(action, scenario) ||
		!strings.HasPrefix(scenario.BaselineRef.Kind, "reviewed_") ||
		!strings.HasPrefix(scenario.AlternativeRef.Kind, "reviewed_") {
		return "", false
	}
	baseline, ok := nonnegativeDecimal(scenario.BaselineEnergy)
	if !ok {
		return "", false
	}
	alternative, ok := nonnegativeDecimal(scenario.AlternativeEnergy)
	if !ok || alternative.Cmp(baseline) > 0 {
		return "", false
	}
	rate, ok := nonnegativeDecimal(scenario.RateUSDPerKWh)
	if !ok || rate.Sign() == 0 {
		return "", false
	}
	saving := new(big.Rat).Sub(baseline, alternative)
	saving.Mul(saving, big.NewRat(scenario.UsagePerWeek*52, 1))
	saving.Mul(saving, rate)
	return saving.FloatString(6), true
}

// The scenario's reviewed baseline must be the same active fact that made
// this action eligible. A different fact for the same model is insufficient.
func hasCurrentBaseline(action Action, scenario BenefitScenario) bool {
	kind := "cycle_energy"
	if action.usageUnit == "hours_per_week" {
		kind = "hourly_energy"
	}
	if scenario.BaselineRef.Kind != "reviewed_usage_fact" {
		return false
	}
	for _, ref := range action.EvidenceRefs {
		if ref.ID == scenario.BaselineRef.ID && ref.Revision == scenario.BaselineRef.Revision && ref.Kind == kind {
			return true
		}
	}
	return false
}

func hasEvidence(haystack []Evidence, needle Evidence) bool {
	for _, ref := range haystack {
		if ref == needle {
			return true
		}
	}
	return false
}

func nonnegativeDecimal(text string) (*big.Rat, bool) {
	if text == "" || strings.Trim(text, "0123456789.") != "" || strings.Count(text, ".") > 1 {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(text)
	return value, ok && value.Sign() >= 0
}
