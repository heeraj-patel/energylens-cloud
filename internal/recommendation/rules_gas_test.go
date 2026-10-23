package recommendation

import (
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"testing"
)

func fp(v float64) *float64 { return &v }

func ev(id, kind string) Evidence { return Evidence{ID: id, Kind: kind, Revision: 1} }

func gasRuleAction(t *testing.T, a Appliance, ruleID string) Action {
	t.Helper()
	for _, action := range EvaluateGasAppliances(a) {
		if action.RuleID == ruleID {
			return action
		}
	}
	t.Fatalf("rule %s missing: %+v", ruleID, EvaluateGasAppliances(a))
	return Action{}
}

func TestGasRulesNeedTheAppliancesCapability(t *testing.T) {
	for _, category := range []string{"furnace", "boiler", "water_heater", "space_heater"} {
		a := Appliance{ID: "x", Category: category, Fuel: "natural_gas", Identity: "confirmed_exact"}
		for name, evaluate := range map[string]func(Appliance) []Action{"legacy": Evaluate, "dryer": EvaluateGasEnabled} {
			if got := evaluate(a); len(got) != 1 || got[0].RuleID != "general.gather_information" {
				t.Fatalf("%s/%s saw gas rules: %+v", category, name, got)
			}
		}
		if got := EvaluateGasAppliances(a); len(got) == 0 || got[0].RuleID == "general.gather_information" {
			t.Fatalf("%s capability on: %+v", category, got)
		}
	}
	dryer := Appliance{ID: "d", Category: "dryer", Fuel: "natural_gas"}
	if got := EvaluateGasAppliances(dryer); len(got) != 1 || got[0].RuleID != "dryer.gas_cycle_options" {
		t.Fatalf("dryer vocabulary lost: %+v", got)
	}
}

func TestHeldGasRulesAreAbsent(t *testing.T) {
	held := map[string]bool{"furnace.gas_filter_care": true, "boiler.gas_service_care": true, "water_heater.gas_temperature": true, "range.gas_cooking": true}
	for _, r := range gasRules {
		if held[r.id] {
			t.Fatalf("held rule present: %s", r.id)
		}
	}
	if got := EvaluateGasAppliances(Appliance{Category: "range", Fuel: "natural_gas", Identity: "confirmed_exact"}); len(got) != 1 || got[0].RuleID != "general.gather_information" {
		t.Fatalf("range: %+v", got)
	}
	if len(gasRules) != 7 {
		t.Fatalf("gas rule count: %d", len(gasRules))
	}
}

func TestGasCheckRuleLadders(t *testing.T) {
	cases := []struct {
		rule, category, kind, noFact, withFact string
		value                                  GasValues
	}{
		{"furnace.gas_efficiency_check", "furnace", "afue", "Add the furnace model to compare its efficiency rating.",
			"This furnace is rated 96% AFUE. ENERGY STAR gas furnaces are 95% or higher (97% in northern states).", GasValues{AFUE: fp(96)}},
		{"boiler.gas_efficiency_check", "boiler", "afue", "Add the boiler model to compare its efficiency rating.",
			"This boiler is rated 91.5% AFUE. ENERGY STAR gas boilers are 90% or higher.", GasValues{AFUE: fp(91.5)}},
		{"water_heater.gas_efficiency_check", "water_heater", "uniform_energy_factor", "Add the water heater model to compare its efficiency rating.",
			"Rated 0.82 UEF. ENERGY STAR gas storage tank models are 0.81 or higher.", GasValues{UEF: fp(0.82), DrawPattern: "medium"}},
	}
	for _, c := range cases {
		for _, fuel := range []string{"natural_gas", "propane"} {
			a := Appliance{ID: "a", Category: c.category, Fuel: fuel}
			if got := gasRuleAction(t, a, c.rule); got.State != "candidate" || got.NextQuestion != "confirm_model" || got.Explanation != c.noFact || got.TipCategory != TipUpgrade {
				t.Fatalf("%s unresolved: %+v", c.rule, got)
			}
			a.Identity = "confirmed_exact"
			if got := gasRuleAction(t, a, c.rule); got.State != "candidate" || got.NextQuestion != "find_reviewed_energy_source" {
				t.Fatalf("%s no fact: %+v", c.rule, got)
			}
			a.Facts, a.Gas = []Evidence{ev("fact", c.kind)}, &c.value
			got := gasRuleAction(t, a, c.rule)
			if got.State != "active" || got.NextQuestion != "" || got.Explanation != c.withFact || len(got.EvidenceRefs) != 1 || got.SavingsAmount != nil {
				t.Fatalf("%s/%s with fact: %+v", c.rule, fuel, got)
			}
		}
	}
}

func TestWaterHeaterEfficiencyReference(t *testing.T) {
	cases := []struct {
		gas  GasValues
		want string
	}{
		{GasValues{UEF: fp(0.88), DrawPattern: "high"}, "Rated 0.88 UEF. ENERGY STAR gas storage tank models are 0.86 or higher."},
		{GasValues{UEF: fp(0.96), Tankless: true, DrawPattern: "high"}, "Rated 0.96 UEF. ENERGY STAR gas tankless models are 0.95 or higher."},
	}
	for _, c := range cases {
		gas := c.gas
		a := Appliance{ID: "w", Category: "water_heater", Fuel: "natural_gas", Identity: "confirmed_exact", Facts: []Evidence{ev("u", "uniform_energy_factor")}, Gas: &gas}
		if got := gasRuleAction(t, a, "water_heater.gas_efficiency_check"); got.Explanation != c.want {
			t.Fatalf("explanation %q", got.Explanation)
		}
	}
}

func TestGasStandbyRuleLadders(t *testing.T) {
	rate := &Evidence{ID: "rate", Kind: "tariff", Revision: 1}
	kwh := 11.0 * 8.76
	cases := []struct {
		rule, category, candidate, active string
		typicalOK                         bool
	}{
		{"furnace.gas_standby", "furnace", "Even when not heating, furnace controls use a little electricity.", "Even when not heating, furnace controls use a little electricity, about $19/yr.", true},
		{"boiler.gas_standby", "boiler", "This boiler's controls use electricity while idle.", "This boiler's controls use about $19/yr in electricity while idle.", false},
	}
	for _, c := range cases {
		a := Appliance{ID: "s", Category: c.category, Fuel: "natural_gas"}
		if got := gasRuleAction(t, a, c.rule); got.NextQuestion != "confirm_model" || got.Explanation != c.candidate || got.TipCategory != TipGeneral {
			t.Fatalf("%s unresolved: %+v", c.rule, got)
		}
		a.Identity = "confirmed_exact"
		a.Gas = &GasValues{StandbyKWh: fp(kwh)}
		typical := gasRuleAction(t, a, c.rule)
		if c.typicalOK {
			if typical.NextQuestion != "add_electricity_rate" || typical.State != "candidate" {
				t.Fatalf("%s typical: %+v", c.rule, typical)
			}
		} else if typical.NextQuestion != "find_reviewed_energy_source" {
			t.Fatalf("%s typical must not count: %+v", c.rule, typical)
		}
		a.Gas = &GasValues{StandbyKWh: fp(kwh), StandbyRated: true}
		a.Facts = []Evidence{ev("sb", "standby_power")}
		if got := gasRuleAction(t, a, c.rule); got.NextQuestion != "add_electricity_rate" || got.State != "candidate" || len(got.EvidenceRefs) != 1 {
			t.Fatalf("%s rated no rate: %+v", c.rule, got)
		}
		a.Tariff, a.ElectricRate = rate, fp(0.2)
		got := gasRuleAction(t, a, c.rule)
		if got.State != "active" || got.Explanation != c.active || len(got.EvidenceRefs) != 2 {
			t.Fatalf("%s active: %+v", c.rule, got)
		}
	}
}

func TestWaterHeaterGasCostLadder(t *testing.T) {
	const rule = "water_heater.gas_cost"
	gasTariff := &Evidence{ID: "gas", Kind: "gas_tariff", Revision: 1}
	a := Appliance{ID: "w", Category: "water_heater", Fuel: "natural_gas"}
	steps := []struct {
		name, state, next, explanation string
		refs                           int
		mutate                         func(*Appliance)
	}{
		{"unresolved", "candidate", "confirm_model", "Add your gas rate in Settings to see what hot water costs you.", 0, func(*Appliance) {}},
		{"no source", "candidate", "find_reviewed_energy_source", "Add your gas rate in Settings to see what hot water costs you.", 0, func(a *Appliance) { a.Identity = "confirmed_exact" }},
		{"household size", "candidate", "household_size", "Tell us how many people live in your home for a closer estimate.", 1, func(a *Appliance) {
			a.Facts = []Evidence{ev("u", "uniform_energy_factor")}
			a.Gas = &GasValues{Therms: fp(200), UEF: fp(0.7)}
		}},
		{"gas rate", "candidate", "add_gas_rate", "Add your gas rate in Settings to see what hot water costs you.", 2, func(a *Appliance) {
			a.Answers = map[string]Evidence{"household_size": ev("hh", "confirmed_answer")}
		}},
		{"active", "active", "", "Heating water costs about $240/yr with your gas rate.", 3, func(a *Appliance) {
			a.GasTariff, a.GasRate = gasTariff, fp(1.2)
		}},
	}
	for _, s := range steps {
		s.mutate(&a)
		got := gasRuleAction(t, a, rule)
		if got.State != s.state || got.NextQuestion != s.next || got.Explanation != s.explanation || len(got.EvidenceRefs) != s.refs || got.SavingsAmount != nil || got.TipCategory != TipGeneral {
			t.Fatalf("%s: %+v", s.name, got)
		}
	}
	a.Answers, a.Gas.HouseholdNotSure = nil, true
	if got := gasRuleAction(t, a, rule); got.State != "active" || len(got.EvidenceRefs) != 2 {
		t.Fatalf("not sure: %+v", got)
	}
	a.Gas.Therms = nil
	if got := gasRuleAction(t, a, rule); got.State != "candidate" || got.NextQuestion != "find_reviewed_energy_source" {
		t.Fatalf("no estimate basis: %+v", got)
	}
	a.Gas.Therms, a.Facts = fp(180), []Evidence{ev("af", "annual_fuel")}
	if got := gasRuleAction(t, a, rule); got.State != "active" || got.Explanation != "Heating water costs about $216/yr with your gas rate." {
		t.Fatalf("annual fuel fact skips the question: %+v", got)
	}
}

func TestPropaneCostRulesStayAtAddGasRate(t *testing.T) {
	a := Appliance{ID: "p", Category: "water_heater", Fuel: "propane", Identity: "confirmed_exact",
		Facts:     []Evidence{ev("u", "uniform_energy_factor")},
		Answers:   map[string]Evidence{"household_size": ev("hh", "confirmed_answer")},
		Gas:       &GasValues{UEF: fp(0.7), Therms: fp(100)},
		GasTariff: &Evidence{ID: "gas", Kind: "gas_tariff", Revision: 1}, GasRate: fp(2)}
	if got := gasRuleAction(t, a, "water_heater.gas_cost"); got.State != "candidate" || got.NextQuestion != "add_gas_rate" {
		t.Fatalf("propane cost: %+v", got)
	}
	heater := Appliance{ID: "h", Category: "space_heater", Fuel: "propane", Identity: "confirmed_exact",
		Facts:   []Evidence{ev("a", "afue")},
		Answers: map[string]Evidence{"hours_per_week": ev("hrs", "confirmed_answer")},
	}
	if got := gasRuleAction(t, heater, "space_heater.gas_schedule"); got.State != "active" || got.NextQuestion != "" {
		t.Fatalf("propane schedule needs no rate: %+v", got)
	}
}

func TestGasSpaceHeaterScheduleLadder(t *testing.T) {
	const rule, copy = "space_heater.gas_schedule", "Use this heater's controls to run it only when the room is in use."
	a := Appliance{ID: "h", Category: "space_heater", Fuel: "natural_gas"}
	steps := []struct {
		state, next string
		refs        int
		mutate      func(*Appliance)
	}{
		{"candidate", "confirm_model", 0, func(*Appliance) {}},
		{"candidate", "find_reviewed_energy_source", 0, func(a *Appliance) { a.Identity = "confirmed_exact" }},
		{"candidate", "hours_per_week", 1, func(a *Appliance) { a.Facts = []Evidence{ev("a", "afue")} }},
		{"active", "", 2, func(a *Appliance) { a.Answers = map[string]Evidence{"hours_per_week": ev("hrs", "confirmed_answer")} }},
	}
	for i, s := range steps {
		s.mutate(&a)
		got := gasRuleAction(t, a, rule)
		if got.State != s.state || got.NextQuestion != s.next || len(got.EvidenceRefs) != s.refs || got.Explanation != copy || got.SavingsAmount != nil {
			t.Fatalf("step %d: %+v", i, got)
		}
	}
}

func TestGasSpaceHeaterTakesNoUntaggedElectricTips(t *testing.T) {
	tip := func(id, fuels string) datawizfacts.Fact {
		value := `{"context":"night","applicable_categories":["hvac"]` + fuels + `}`
		return datawizfacts.Fact{ID: id, Field: "efficiency_tip", Value: json.RawMessage(value)}
	}
	a := Appliance{ID: "h", Category: "space_heater", Fuel: "natural_gas", Identity: "confirmed_exact",
		ResearchTips: []datawizfacts.Fact{tip("untagged", ""), tip("gas", `,"applicable_fuels":["natural_gas"]`)}}
	var attached []string
	for _, action := range EvaluateGasAppliances(a) {
		for _, f := range action.ResearchEvidence {
			attached = append(attached, f.ID)
		}
	}
	if len(attached) != 1 || attached[0] != "gas" {
		t.Fatalf("attached: %v", attached)
	}
}

func TestEvaluatorForCapability(t *testing.T) {
	a := Appliance{ID: "f", Category: "furnace", Fuel: "natural_gas"}
	for capability, want := range map[string]string{
		"": "general.gather_information", "gas_dryer_recommendations_v1": "general.gather_information",
		"other": "general.gather_information", "gas_appliance_recommendations_v1": "furnace.gas_efficiency_check"} {
		got := evaluatorFor(capability)(a)
		found := false
		for _, action := range got {
			found = found || action.RuleID == want
		}
		if !found {
			t.Fatalf("capability %q: %+v", capability, got)
		}
	}
}
