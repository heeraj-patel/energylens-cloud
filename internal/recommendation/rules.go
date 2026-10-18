// Package recommendation selects evidence-bounded actions for an appliance.
// Rules here suggest customer actions; they never control a device or infer
// savings from a nameplate, annual test figure, or unsupported usage pattern.
package recommendation

import (
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"math/big"
	"sort"
	"strings"
)

type Evidence struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

type Appliance struct {
	ResearchTips     []datawizfacts.Fact
	ID               string
	Category         string
	categoryKey      string
	ruleCategoryKeys map[string]string
	ruleKeyCounts    map[string]int
	Fuel             string
	Identity         string // confirmed_exact, documented_variant_family, unresolved
	Facts            []Evidence
	Answers          map[string]Evidence // only confirmed, current appliance-revision answers
	Tariff           *Evidence           // current marginal electricity rate; never used for a savings claim
	GasUsage         *Evidence           // exact reviewed dryer therm/cycle fact
	GasTariff        *Evidence           // fresh USD/therm marginal rate
	UsageFrequency   int64               // legacy frequency for callers supplying one answer kind
	HoursPerWeek     *int64              // confirmed current hours per week, including zero
	CyclesPerWeek    *int64              // confirmed current cycles per week, including zero
	Scenarios        []BenefitScenario   // explicit current action alternatives
}

func (a Appliance) matchesRuleCategory(ruleCategory string) bool {
	if a.Category == ruleCategory {
		return true
	}
	key := a.ruleCategoryKeys[ruleCategory]
	return key != "" && key == a.categoryKey && a.ruleKeyCounts[key] == 1
}

func (a Appliance) frequencyFor(kind string) int64 {
	switch kind {
	case "hours_per_week":
		if a.HoursPerWeek != nil {
			return *a.HoursPerWeek
		}
	case "cycles_per_week":
		if a.CyclesPerWeek != nil {
			return *a.CyclesPerWeek
		}
	}
	return a.UsageFrequency
}

type Action struct {
	ResearchEvidence []datawizfacts.Fact `json:"research_evidence,omitempty"`
	RuleID           string              `json:"rule_id"`
	RuleVersion      int                 `json:"rule_version"`
	ApplianceID      string              `json:"appliance_id"`
	State            string              `json:"state"` // candidate or active
	Title            string              `json:"title"`
	Explanation      string              `json:"explanation"`
	NextQuestion     string              `json:"next_question,omitempty"`
	BenefitGroup     string              `json:"benefit_group"`
	TipCategory      TipCategory         `json:"tip_category"`
	EvidenceRefs     []Evidence          `json:"evidence_refs"`
	SavingsAmount    *string             `json:"savings_amount"` // absent until a comparable scenario exists
	SavingsPeriod    string              `json:"savings_period,omitempty"`
	SavingsCurrency  string              `json:"savings_currency,omitempty"`
	SavingsBasis     string              `json:"savings_basis,omitempty"`
	ScenarioRefs     []Evidence          `json:"scenario_refs,omitempty"`
	SourceURL        string              `json:"source_url,omitempty"`
	usageFrequency   int64
	usageUnit        string
}

// TipCategory identifies the kind of action at its source. BenefitGroup is
// reserved for appliance-scoped ranking and does not determine this category.
type TipCategory string

const (
	TipGeneral TipCategory = "general"
	TipCare    TipCategory = "care"
	TipUpgrade TipCategory = "upgrade"
)

func tipCategory(ruleID string) TipCategory {
	switch ruleID {
	case "refrigerator.maintenance":
		return TipCare
	case "lighting.equivalent_bulb":
		return TipUpgrade
	default:
		return TipGeneral
	}
}

type rule struct {
	id, category, fuel, title, explanation, question, group string
	answerKind, factKind                                    string
}

// These initial rules only guide source- and usage-gathering or low-risk
// settings checks. A separate scenario calculator must establish any savings.
var initial = []rule{
	{"computer.sleep_settings", "computer", "electricity", "Review sleep settings", "Check that the computer and display enter a manufacturer-supported sleep mode when idle.", "hours_per_week", "computer_idle", "hours_per_week", "annual_energy"},
	{"television.quick_start", "television", "electricity", "Review quick-start settings", "Check whether quick-start or network standby is needed, and use the manufacturer's supported settings.", "hours_per_week", "entertainment_standby", "hours_per_week", "annual_energy"},
	{"washer.cycle_options", "washer", "electricity", "Review wash-cycle options", "Compare manufacturer-supported cycle and temperature options while keeping the wash service you need.", "cycles_per_week", "laundry_cycle", "cycles_per_week", "cycle_energy"},
	{"dryer.cycle_options", "dryer", "electricity", "Review dryer cycle options", "Check manufacturer-supported sensor-dry and lower-heat options for suitable loads.", "cycles_per_week", "laundry_cycle", "cycles_per_week", "cycle_energy"},
	{"refrigerator.maintenance", "refrigerator", "electricity", "Review refrigerator setup", "Check the manufacturer's clearance, seal and temperature guidance before changing settings.", "", "refrigeration", "", "annual_energy"},
	{"air_conditioner.schedule", "air_conditioner", "electricity", "Review cooling schedule", "Consider a comfort-safe schedule using controls supported by this unit.", "hours_per_week", "cooling_schedule", "hours_per_week", "annual_energy"},
	{"space_heater.schedule", "space_heater", "electricity", "Review heating schedule", "Consider a comfort-safe schedule using controls supported by this unit.", "hours_per_week", "heating_schedule", "hours_per_week", "annual_energy"},
	{"dishwasher.cycle_options", "dishwasher", "electricity", "Review dishwasher cycle options", "Check the manual for an eco or air-dry option suitable for your dishes; keep cleaning and drying needs in mind.", "cycles_per_week", "dishwasher_cycle", "cycles_per_week", "cycle_energy"},
	{"heat_pump.operating_guidance", "heat_pump", "electricity", "Review heat-pump operating guidance", "Check manufacturer guidance for supported modes and maintenance while protecting comfort and backup-heat needs.", "", "hvac_operation", "", "annual_energy"},
	{"water_heater.operating_mode", "water_heater", "electricity", "Review water-heater operating mode", "Check the manufacturer's supported mode and temperature guidance. Keep sanitation and hot-water needs protected.", "", "water_heating", "", "annual_energy"},
	{"dehumidifier.humidity_setting", "dehumidifier", "electricity", "Review humidity setting", "Check the manufacturer's supported humidity setting while maintaining safe moisture control.", "", "humidity_control", "", "annual_energy"},
	{"lighting.equivalent_bulb", "lighting", "electricity", "Review equivalent lighting options", "Check whether a manufacturer-supported efficient bulb provides the light and fixture compatibility you need.", "", "lighting_service", "", "annual_energy"},
	{"fan.speed_setting", "fan", "electricity", "Review fan settings", "Check supported speed and timer settings while maintaining the ventilation or comfort you need.", "", "fan_service", "", "annual_energy"},
	{"pool_pump.operating_guidance", "pool_pump", "electricity", "Review pool-pump operating guidance", "Check the equipment manual and water-care requirements before considering supported operating settings.", "", "pool_service", "", "annual_energy"},
}

// Evaluate returns at most one action per rule. Source facts and current
// confirmed answers make an action active; otherwise it remains a candidate
// with an explicit next question. Savings require a separate reviewed action
// scenario. An annual fact alone never establishes a usage-based saving.
func Evaluate(a Appliance) []Action {
	return evaluate(a, false)
}

// EvaluateGasEnabled is used only for clients that explicitly accept the gas
// dryer rule and follow-up prompt. Legacy recommendation pages keep their
// original vocabulary and their gather-information fallback.
func EvaluateGasEnabled(a Appliance) []Action {
	return evaluate(a, true)
}

func evaluate(a Appliance, gasEnabled bool) []Action {
	out := []Action{}
	if gasEnabled && a.Category == "dryer" && a.Fuel == "natural_gas" {
		refs := []Evidence{}
		state, next := "candidate", "confirm_model"
		if a.Identity == "confirmed_exact" {
			next = "find_reviewed_energy_source"
			if a.GasUsage != nil && a.GasUsage.ID != "" && a.GasUsage.Kind == "gas_cycle_energy" && a.GasUsage.Revision > 0 {
				refs = append(refs, *a.GasUsage)
				next = "cycles_per_week"
				if answer, ok := a.Answers["cycles_per_week"]; ok && answer.ID != "" && answer.Kind == "confirmed_answer" && answer.Revision > 0 {
					refs = append(refs, answer)
					next = "add_gas_rate"
					if a.GasTariff != nil && a.GasTariff.ID != "" && a.GasTariff.Kind == "gas_tariff" && a.GasTariff.Revision > 0 {
						refs = append(refs, *a.GasTariff)
						state, next = "active", ""
					}
				}
			}
		}
		out = append(out, Action{RuleID: "dryer.gas_cycle_options", RuleVersion: 1, ApplianceID: a.ID,
			State: state, Title: "Review gas dryer cycle options",
			Explanation:  "Check manufacturer-supported sensor-dry and suitable load settings. Gas and electrical use need separate evidence; this action does not identify waste or establish savings.",
			NextQuestion: next, BenefitGroup: "laundry_cycle", TipCategory: TipGeneral, EvidenceRefs: refs, usageFrequency: a.frequencyFor("cycles_per_week"), usageUnit: "cycles_per_week"})
	}
	for _, r := range initial {
		if !a.matchesRuleCategory(r.category) || a.Fuel != r.fuel {
			continue
		}
		refs := []Evidence{}
		if a.Identity == "confirmed_exact" {
			for _, fact := range a.Facts {
				if fact.Kind == r.factKind && fact.ID != "" && fact.Revision > 0 {
					refs = append(refs, fact)
					break
				}
			}
		}
		state, next := "candidate", "confirm_model"
		if a.Identity == "confirmed_exact" {
			next = "find_reviewed_energy_source"
		}
		if len(refs) > 0 && r.answerKind != "" {
			if answer, ok := a.Answers[r.answerKind]; ok && answer.ID != "" && answer.Revision > 0 {
				refs = append(refs, answer)
			} else {
				next = r.question
			}
		}
		if len(refs) > 0 && (r.answerKind == "" || len(refs) > 1) {
			if a.Tariff != nil && a.Tariff.ID != "" && a.Tariff.Kind == "tariff" && a.Tariff.Revision > 0 {
				refs = append(refs, *a.Tariff)
				state, next = "active", ""
			} else {
				next = "add_electricity_rate"
			}
		}
		out = append(out, Action{RuleID: r.id, RuleVersion: 1, ApplianceID: a.ID, State: state,
			Title: r.title, Explanation: r.explanation, NextQuestion: next,
			BenefitGroup: r.group, TipCategory: tipCategory(r.id), EvidenceRefs: refs, usageFrequency: a.frequencyFor(r.answerKind), usageUnit: r.answerKind})
	}
	if len(out) == 0 {
		out = append(out, Action{RuleID: "general.gather_information", RuleVersion: 1, ApplianceID: a.ID,
			State: "candidate", Title: "Gather appliance information",
			Explanation:  "Add the appliance model and reviewed guidance to see whether a specific action applies.",
			NextQuestion: "confirm_model", BenefitGroup: "information", TipCategory: TipGeneral, EvidenceRefs: []Evidence{}})
	}
	for i := range out {
		for _, tip := range a.ResearchTips {
			if tip.Field != "efficiency_tip" {
				continue
			}
			var value struct {
				Context    string   `json:"context"`
				Applicable []string `json:"applicable_categories"`
				Excluded   []string `json:"excluded_categories"`
			}
			if json.Unmarshal(tip.Value, &value) != nil || !tipApplies(value.Context, a.Category, out[i].RuleID, value.Applicable, value.Excluded) {
				continue
			}
			out[i].ResearchEvidence = append(out[i].ResearchEvidence, tip)
			out[i].EvidenceRefs = append(out[i].EvidenceRefs, Evidence{ID: tip.ID, Kind: "datawiz_efficiency_tip", Revision: 1})
		}
	}
	return rankActions(ApplyBenefitScenarios(out, a.Scenarios))
}

// Source guidance can support an existing action without asserting that its
// prerequisites, household applicability, or numeric benefit are established.
func tipApplies(context, category, ruleID string, applicable, excluded []string) bool {
	aliases := map[string][]string{"hvac": {"heat_pump", "air_conditioner", "space_heater"}, "smart_home_devices": {"smart_home_devices", "smart_plug", "thermostat"}}
	matches := func(name string) bool {
		if name == category {
			return true
		}
		for _, alias := range aliases[name] {
			if alias == category {
				return true
			}
		}
		return false
	}
	for _, name := range excluded {
		if matches(name) {
			return false
		}
	}
	matched := false
	for _, name := range applicable {
		if matches(name) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	switch context {
	case "night", "away":
		return strings.Contains(ruleID, "schedule") || strings.Contains(ruleID, "operating_guidance") || ruleID == "general.gather_information"
	case "peak_usage":
		return ruleID == "general.gather_information" || strings.Contains(ruleID, "settings")
	case "maintenance":
		return strings.Contains(ruleID, "maintenance") || strings.Contains(ruleID, "operating_guidance") || ruleID == "general.gather_information"
	case "habits", "appliance_type", "upgrades":
		return true
	default:
		return false
	}
}

// rankActions keeps every action. It first applies the evidence/activity order,
// then sorts only supplied savings values that share an explicit comparison
// period, currency, and benefit basis. Actions without comparable values keep
// their position and remain visible.
func rankActions(actions []Action) []Action {
	ranked := append([]Action(nil), actions...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.State != b.State {
			return a.State == "active"
		}
		if a.State == "candidate" && readiness(a) != readiness(b) {
			return readiness(a) > readiness(b)
		}
		// Confirmed household frequency orders otherwise eligible actions.
		// It is an opportunity proxy, never a savings or cross-fuel benefit.
		if a.State == "active" {
			if a.usageUnit != b.usageUnit {
				return a.usageUnit < b.usageUnit
			}
			if a.usageUnit != "" && a.usageFrequency != b.usageFrequency {
				return a.usageFrequency > b.usageFrequency
			}
		}
		if a.ApplianceID != b.ApplianceID {
			return a.ApplianceID < b.ApplianceID
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		return a.BenefitGroup < b.BenefitGroup
	})
	return rankComparableBenefits(ranked)
}

func rankComparableBenefits(actions []Action) []Action {
	groups := make(map[string][]int)
	for i, action := range actions {
		if _, ok := comparableSaving(action); !ok {
			continue
		}
		key := action.State + "\x00" + action.SavingsPeriod + "\x00" + action.SavingsCurrency + "\x00" + action.SavingsBasis
		groups[key] = append(groups[key], i)
	}
	for _, positions := range groups {
		values := make([]Action, len(positions))
		for i, position := range positions {
			values[i] = actions[position]
		}
		sort.SliceStable(values, func(i, j int) bool {
			left, _ := comparableSaving(values[i])
			right, _ := comparableSaving(values[j])
			return left.Cmp(right) > 0
		})
		for i, position := range positions {
			actions[position] = values[i]
		}
	}
	return actions
}

func comparableSaving(action Action) (*big.Rat, bool) {
	if action.SavingsAmount == nil || strings.TrimSpace(*action.SavingsAmount) != *action.SavingsAmount ||
		action.SavingsPeriod == "" || action.SavingsCurrency == "" || action.SavingsBasis == "" || len(action.ScenarioRefs) == 0 {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(*action.SavingsAmount)
	return value, ok && value.Sign() >= 0
}

func readiness(action Action) int {
	switch action.NextQuestion {
	case "add_electricity_rate", "add_gas_rate":
		return 3
	case "hours_per_week", "cycles_per_week":
		return 2
	case "find_reviewed_energy_source":
		return 1
	default:
		return 0
	}
}
