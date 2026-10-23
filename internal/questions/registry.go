// Package questions keeps versioned usage prompts and immutable answers.
package questions

import "strings"

type Question struct {
	ID               string   `json:"id"`
	Version          int64    `json:"version"`
	Purpose          string   `json:"purpose"`
	ApplianceID      string   `json:"appliance_id"`
	IdentityRevision int64    `json:"identity_revision"`
	Prompt           string   `json:"prompt"`
	AnswerKind       string   `json:"answer_kind"`
	Unit             string   `json:"unit"`
	Options          []Option `json:"options,omitempty"`
	Optional         bool     `json:"optional"`
	SourceFactID     string   `json:"source_fact_id,omitempty"`
	CycleDefinition  string   `json:"cycle_definition,omitempty"`
	TestBasis        string   `json:"test_basis,omitempty"`
	SourceURL        string   `json:"source_url,omitempty"`
}

// ReviewedCycleQuestionID binds an answer to one immutable reviewed fact.
// Replacing that fact produces a new question and cannot reuse an old count.
func ReviewedCycleQuestionID(category, factID string) string {
	return category + ".reviewed_cycle." + strings.ToLower(factID) + ".cycles_per_week"
}

type Option struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
}

type definition struct {
	Category, ID, Kind, Prompt, Unit string
	Options                          []Option
}

var (
	hoursOptions = []Option{{"lt_1h", 3.5}, {"1_3h", 14}, {"3_5h", 28}, {"5_8h", 45.5}, {"gt_8h", 70}}
	cycleOptions = []Option{{"0_2", 1.5}, {"3_5", 4}, {"6_8", 7}, {"9_plus", 10}}
	seasonOpts   = []Option{{"all_year", 1}, {"6_months", 0.5}, {"3_months", 0.25}, {"1_month", 0.083}}
	// DOE 2014 standard = 1.0; 2001 standard ~1.2-1.4x (DOE 2011 final rule govinfo 2011-22329); 2008-2015 mixes both.
	ageOptions   = []Option{{"2016_or_newer", 1}, {"2008_2015", 1.25}, {"2007_or_older", 1.3}}
	spaceOptions = []Option{{"no", 0}, {"yes", 1}}
	// Hot gal/day for 2, 4 and 6 people: Building America fixture method as in LBNL Home Energy Score, 67 F rise.
	householdOptions = []Option{{"people_0_2", 38}, {"people_3_5", 64}, {"people_6_plus", 91}}
)

const (
	hoursPrompt     = "About how many hours a day is this on?"
	tvPrompt        = "About how many hours a day is this TV on?"
	seasonPrompt    = "How much of the year do you use it?"
	loadsPrompt     = "About how many loads a week?"
	agePrompt       = "About when was it made?"
	garagePrompt    = "Is it in a garage or unconditioned space?"
	householdPrompt = "How many people live in your home?"
	hotWaterUnit    = "gallons/day"
	hoursUnit       = "hours/week"
	cyclesUnit      = "cycles/week"
	seasonUnit      = "fraction of year"
	ageUnit         = "energy factor"
	flagUnit        = "flag"
)

func hours(category, id, prompt string) definition {
	return definition{category, id + ".hours_per_week", "choice", prompt, hoursUnit, hoursOptions}
}

func cycles(category, prompt string) definition {
	return definition{category, category + ".cycles_per_week", "choice", prompt, cyclesUnit, cycleOptions}
}

func season(category string) definition {
	return definition{category, category + ".season_share", "choice", seasonPrompt, seasonUnit, seasonOpts}
}

func fridge(category string) []definition {
	return []definition{
		{category, category + ".age_band", "choice", agePrompt, ageUnit, ageOptions},
		{category, category + ".unconditioned_space", "choice", garagePrompt, flagUnit, spaceOptions},
	}
}

const onboardingVersion = 2

// Boiler reuses the hvac question ids so answers survive a hvac/boiler recategorisation.
var onboarding = map[string][]definition{
	"light":           {hours("light", "lighting", hoursPrompt)},
	"lighting":        {hours("lighting", "lighting", hoursPrompt)},
	"washer":          {cycles("washer", loadsPrompt)},
	"dryer":           {cycles("dryer", loadsPrompt)},
	"dishwasher":      {cycles("dishwasher", loadsPrompt)},
	"computer":        {hours("computer", "computer", hoursPrompt)},
	"monitor":         {hours("monitor", "monitor", hoursPrompt)},
	"television":      {hours("television", "television", tvPrompt)},
	"tv":              {hours("tv", "television", tvPrompt)},
	"space_heater":    {hours("space_heater", "space_heater", hoursPrompt), season("space_heater")},
	"air_conditioner": {hours("air_conditioner", "air_conditioner", hoursPrompt)},
	"fan":             {hours("fan", "fan", hoursPrompt), season("fan")},
	"dehumidifier":    {hours("dehumidifier", "dehumidifier", hoursPrompt), season("dehumidifier")},
	"hvac":            {hours("hvac", "hvac", hoursPrompt), season("hvac")},
	"boiler":          {hours("boiler", "hvac", hoursPrompt), {"boiler", "hvac.season_share", "choice", seasonPrompt, seasonUnit, seasonOpts}},
	"water_heater":    {{"water_heater", "water_heater.household_size", "choice", householdPrompt, hotWaterUnit, householdOptions}},
	"refrigerator":    fridge("refrigerator"),
	"freezer":         fridge("freezer"),
}

// askedForFuel hides questions that only change a gas estimate for other fuels.
func askedForFuel(questionID, fuel string) bool {
	if questionID == "water_heater.household_size" {
		return fuel == "natural_gas" || fuel == "propane"
	}
	return true
}

func isUsageQuestion(id string) bool {
	for _, defs := range onboarding {
		for _, q := range defs {
			if q.ID == id {
				return true
			}
		}
	}
	return false
}

// These are self-reported context prompts. Answers do not classify waste or
// establish the cause of a bill, and they do not imply numeric savings.
var avoid = map[string]definition{
	"computer":   {"computer", "avoid.computer.unused_on_hours_per_week", "hours_per_week", "In a typical week, how many hours is this computer powered on when no one is using it?", "hours/week", nil},
	"television": {"television", "avoid.television.unused_on_hours_per_week", "hours_per_week", "In a typical week, how many hours is this television powered on when no one is watching it?", "hours/week", nil},
}

// FAQ asks recent use rather than treating an annual rating or a usual-use
// estimate as evidence of what happened to a customer's bill.
var faq = map[string]definition{
	"washer":          {"washer", "faq-contributors/v1.washer.recent_cycles", "cycles_per_week", "In the past 7 days, how many wash cycles did you run?", "cycles/week", nil},
	"dryer":           {"dryer", "faq-contributors/v1.dryer.recent_cycles", "cycles_per_week", "In the past 7 days, how many dryer cycles did you run?", "cycles/week", nil},
	"dishwasher":      {"dishwasher", "faq-contributors/v1.dishwasher.recent_cycles", "cycles_per_week", "In the past 7 days, how many dishwasher cycles did you run?", "cycles/week", nil},
	"computer":        {"computer", "faq-contributors/v1.computer.recent_hours", "hours_per_week", "In the past 7 days, how many hours was this computer in use?", "hours/week", nil},
	"television":      {"television", "faq-contributors/v1.television.recent_hours", "hours_per_week", "In the past 7 days, how many hours was this television in use?", "hours/week", nil},
	"space_heater":    {"space_heater", "faq-contributors/v1.space_heater.recent_hours", "hours_per_week", "In the past 7 days, how many hours did you run this heater?", "hours/week", nil},
	"air_conditioner": {"air_conditioner", "faq-contributors/v1.air_conditioner.recent_hours", "hours_per_week", "In the past 7 days, how many hours did you run this air conditioner?", "hours/week", nil},
}

func questionsFor(category, appliance string, version int64, purpose string) []Question {
	var defs []definition
	qversion := int64(1)
	switch purpose {
	case "onboarding":
		defs, qversion = onboarding[category], onboardingVersion
	case "avoid":
		if d, ok := avoid[category]; ok {
			defs = []definition{d}
		}
	case "faq":
		if d, ok := faq[category]; ok {
			defs = []definition{d}
		}
	}
	out := make([]Question, 0, len(defs))
	for _, d := range defs {
		out = append(out, Question{ID: d.ID, Version: qversion, Purpose: purpose, ApplianceID: appliance, IdentityRevision: version, Prompt: d.Prompt, AnswerKind: d.Kind, Unit: d.Unit, Options: d.Options, Optional: true})
	}
	return out
}

func questionByID(category, appliance string, version int64, id string) (Question, bool) {
	for _, purpose := range []string{"onboarding", "avoid", "faq"} {
		for _, q := range questionsFor(category, appliance, version, purpose) {
			if q.ID == id {
				return q, true
			}
		}
	}
	return Question{}, false
}

func questionsForMapped(raw, canonical, appliance string, version int64, purpose string) []Question {
	if qs := questionsFor(raw, appliance, version, purpose); len(qs) > 0 {
		return qs
	}
	return questionsFor(canonical, appliance, version, purpose)
}

func questionByIDMapped(raw, canonical, appliance string, version int64, id string) (Question, bool) {
	if q, ok := questionByID(raw, appliance, version, id); ok {
		return q, true
	}
	return questionByID(canonical, appliance, version, id)
}

// Applies reports whether the question still exists for an appliance of this category.
func Applies(raw, canonical, questionID string) bool {
	_, ok := questionByIDMapped(raw, canonical, "", 0, questionID)
	return ok
}
