// Package questions keeps versioned usage prompts and immutable answers.
package questions

type Question struct {
	ID               string `json:"id"`
	Version          int64  `json:"version"`
	Purpose          string `json:"purpose"`
	ApplianceID      string `json:"appliance_id"`
	IdentityRevision int64  `json:"identity_revision"`
	Prompt           string `json:"prompt"`
	AnswerKind       string `json:"answer_kind"`
	Unit             string `json:"unit"`
	Optional         bool   `json:"optional"`
}

type definition struct {
	Category, ID, Kind, Prompt, Unit string
}

var onboarding = map[string]definition{
	"washer":          {"washer", "washer.cycles_per_week", "cycles_per_week", "How many wash cycles do you usually run each week?", "cycles/week"},
	"dryer":           {"dryer", "dryer.cycles_per_week", "cycles_per_week", "How many dryer cycles do you usually run each week?", "cycles/week"},
	"dishwasher":      {"dishwasher", "dishwasher.cycles_per_week", "cycles_per_week", "How many dishwasher cycles do you usually run each week?", "cycles/week"},
	"computer":        {"computer", "computer.hours_per_week", "hours_per_week", "How many hours do you usually use this computer each week?", "hours/week"},
	"television":      {"television", "television.hours_per_week", "hours_per_week", "How many hours do you usually watch this television each week?", "hours/week"},
	"space_heater":    {"space_heater", "space_heater.hours_per_week", "hours_per_week", "How many hours do you usually run this heater each week?", "hours/week"},
	"air_conditioner": {"air_conditioner", "air_conditioner.hours_per_week", "hours_per_week", "How many hours do you usually run this air conditioner each week?", "hours/week"},
}

// These are self-reported context prompts. Answers do not classify waste or
// establish the cause of a bill, and they do not imply numeric savings.
var avoid = map[string]definition{
	"computer":   {"computer", "avoid.computer.unused_on_hours_per_week", "hours_per_week", "In a typical week, how many hours is this computer powered on when no one is using it?", "hours/week"},
	"television": {"television", "avoid.television.unused_on_hours_per_week", "hours_per_week", "In a typical week, how many hours is this television powered on when no one is watching it?", "hours/week"},
}

// FAQ asks recent use rather than treating an annual rating or a usual-use
// estimate as evidence of what happened to a customer's bill.
var faq = map[string]definition{
	"washer":          {"washer", "faq-contributors/v1.washer.recent_cycles", "cycles_per_week", "In the past 7 days, how many wash cycles did you run?", "cycles/week"},
	"dryer":           {"dryer", "faq-contributors/v1.dryer.recent_cycles", "cycles_per_week", "In the past 7 days, how many dryer cycles did you run?", "cycles/week"},
	"dishwasher":      {"dishwasher", "faq-contributors/v1.dishwasher.recent_cycles", "cycles_per_week", "In the past 7 days, how many dishwasher cycles did you run?", "cycles/week"},
	"computer":        {"computer", "faq-contributors/v1.computer.recent_hours", "hours_per_week", "In the past 7 days, how many hours was this computer in use?", "hours/week"},
	"television":      {"television", "faq-contributors/v1.television.recent_hours", "hours_per_week", "In the past 7 days, how many hours was this television in use?", "hours/week"},
	"space_heater":    {"space_heater", "faq-contributors/v1.space_heater.recent_hours", "hours_per_week", "In the past 7 days, how many hours did you run this heater?", "hours/week"},
	"air_conditioner": {"air_conditioner", "faq-contributors/v1.air_conditioner.recent_hours", "hours_per_week", "In the past 7 days, how many hours did you run this air conditioner?", "hours/week"},
}

func questionFor(category, appliance string, version int64, purpose string) (Question, bool) {
	var registry map[string]definition
	switch purpose {
	case "onboarding":
		registry = onboarding
	case "avoid":
		registry = avoid
	case "faq":
		registry = faq
	default:
		return Question{}, false
	}
	d, ok := registry[category]
	if !ok {
		return Question{}, false
	}
	return Question{ID: d.ID, Version: 1, Purpose: purpose, ApplianceID: appliance, IdentityRevision: version, Prompt: d.Prompt, AnswerKind: d.Kind, Unit: d.Unit, Optional: true}, true
}

func questionByID(category, appliance string, version int64, id string) (Question, bool) {
	for _, purpose := range []string{"onboarding", "avoid", "faq"} {
		if q, ok := questionFor(category, appliance, version, purpose); ok && q.ID == id {
			return q, true
		}
	}
	return Question{}, false
}
