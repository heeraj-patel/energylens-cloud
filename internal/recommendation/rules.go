// Package recommendation selects evidence-bounded actions for an appliance.
// Rules here suggest customer actions; they never control a device or infer
// savings from a nameplate, annual test figure, or unsupported usage pattern.
package recommendation

import "sort"

type Evidence struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

type Appliance struct {
	ID       string
	Category string
	Fuel     string
	Identity string // confirmed_exact, documented_variant_family, unresolved
	Facts    []Evidence
	Answers  map[string]Evidence // only confirmed, current appliance-revision answers
	Tariff   *Evidence           // current marginal electricity rate; never used for a savings claim
}

type Action struct {
	RuleID        string     `json:"rule_id"`
	RuleVersion   int        `json:"rule_version"`
	ApplianceID   string     `json:"appliance_id"`
	State         string     `json:"state"` // candidate or active
	Title         string     `json:"title"`
	Explanation   string     `json:"explanation"`
	NextQuestion  string     `json:"next_question,omitempty"`
	BenefitGroup  string     `json:"benefit_group"`
	EvidenceRefs  []Evidence `json:"evidence_refs"`
	SavingsAmount *string    `json:"savings_amount"` // absent until a comparable scenario exists
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

// Evaluate returns at most one action per rule and leaves all savings amounts
// unknown. Source facts and current confirmed answers make an action active;
// otherwise it remains a candidate with an explicit next question. A reviewed
// annual fact supports only the appliance identity, never a usage-based saving.
func Evaluate(a Appliance) []Action {
	out := []Action{}
	for _, r := range initial {
		if a.Category != r.category || a.Fuel != r.fuel {
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
			BenefitGroup: r.group, EvidenceRefs: refs})
	}
	if len(out) == 0 {
		out = append(out, Action{RuleID: "general.gather_information", RuleVersion: 1, ApplianceID: a.ID,
			State: "candidate", Title: "Gather appliance information",
			Explanation:  "Add the appliance model and reviewed guidance to see whether a specific action applies.",
			NextQuestion: "confirm_model", BenefitGroup: "information", EvidenceRefs: []Evidence{}})
	}
	return rankActions(out)
}

// rankActions keeps the strongest action in each appliance's benefit group and
// limits a single appliance to three actions. A benefit group is scoped to an
// appliance: two different appliances may each have an annual-energy action.
// With no comparable scenario or user goals, this order reflects evidence
// readiness only, never expected savings or a claimed financial benefit.
func rankActions(actions []Action) []Action {
	const maxPerAppliance = 3
	ranked := append([]Action(nil), actions...)
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.State != b.State {
			return a.State == "active"
		}
		if a.State == "candidate" && readiness(a) != readiness(b) {
			return readiness(a) > readiness(b)
		}
		if a.ApplianceID != b.ApplianceID {
			return a.ApplianceID < b.ApplianceID
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		return a.BenefitGroup < b.BenefitGroup
	})
	seen := make(map[string]bool, len(ranked))
	count := make(map[string]int)
	out := make([]Action, 0, len(ranked))
	for _, action := range ranked {
		if count[action.ApplianceID] >= maxPerAppliance {
			continue
		}
		key := action.ApplianceID + "\x00" + action.BenefitGroup
		if action.BenefitGroup != "" && seen[key] {
			continue
		}
		seen[key] = true
		count[action.ApplianceID]++
		out = append(out, action)
	}
	return out
}

func readiness(action Action) int {
	switch action.NextQuestion {
	case "add_electricity_rate":
		return 3
	case "hours_per_week", "cycles_per_week":
		return 2
	case "find_reviewed_energy_source":
		return 1
	default:
		return 0
	}
}
