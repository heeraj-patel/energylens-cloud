// Package faq renders bounded answers from verified facts. It does not compare
// bills or assert the cause of a change in a customer's bill.
package faq

import (
	"slices"
	"sort"
	"time"
)

const (
	Insufficient   = "More usage information is needed."
	NoWeatherTips  = "No weather tips apply right now."
	weatherTipsFAQ = "weather_tips"
)

type Evidence struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

type Finding struct {
	Label       string     `json:"label"` // observation, estimate or hypothesis
	Text        string     `json:"text"`
	EvidenceRef []Evidence `json:"evidence_refs"`
	SourceURL   string     `json:"source_url,omitempty"`
}

type Input struct {
	QuestionID       string
	OperatingCost    *OperatingCost
	ObservedPatterns []Finding
	Actions          []Finding
	Programs         []Finding
	WeatherFindings  []Finding
	// WeatherUnavailable makes weather_tips answer with the insufficient-information fallback.
	WeatherUnavailable bool
	AsOf             time.Time
}

type OperatingCost struct {
	Amount      string
	Currency    string
	PeriodStart string
	PeriodEnd   string
	Evidence    []Evidence
}

type NextAction struct {
	Kind   string `json:"kind"`
	FlowID string `json:"flow_id"`
}

type Answer struct {
	QuestionID    string      `json:"question_id"`
	Status        string      `json:"status"`
	Text          string      `json:"answer"`
	Findings      []Finding   `json:"findings"`
	NextAction    *NextAction `json:"next_action,omitempty"`
	EvidenceRefs  []Evidence  `json:"evidence_refs"`
	AsOf          string      `json:"as_of"`
	ExpiresAt     string      `json:"expires_at,omitempty"`
	InputSnapshot []Evidence  `json:"input_snapshot,omitempty"`
	WeatherStatus string      `json:"weather_status,omitempty"`
	WeatherReason string      `json:"weather_reason,omitempty"`
}

var intents = map[string]bool{
	"high_bill_possible_contributors": true,
	"top_waste_opportunities":         true,
	"device_operating_cost":           true,
	"reduce_cost":                     true,
	"programs_for_device":             true,
	weatherTipsFAQ:                    true,
}

// Render uses only supplied evidence-backed findings. A high-bill answer is
// explicitly about possible contributors in observed history, never a claim
// that a bill was compared. Unknown data always produces the exact fallback.
func Render(in Input) (Answer, bool) {
	if !intents[in.QuestionID] {
		return Answer{}, false
	}
	out := Answer{QuestionID: in.QuestionID, Status: "insufficient_information", Text: Insufficient,
		Findings: []Finding{}, EvidenceRefs: []Evidence{}, AsOf: in.AsOf.UTC().Format(time.RFC3339)}
	var findings []Finding
	switch in.QuestionID {
	case "high_bill_possible_contributors":
		findings = in.ObservedPatterns
	case "top_waste_opportunities", "reduce_cost":
		findings = in.Actions
	case "programs_for_device":
		findings = in.Programs
	case weatherTipsFAQ:
		findings = in.WeatherFindings
	case "device_operating_cost":
		if c := in.OperatingCost; c != nil && c.Amount != "" && c.Currency != "" && c.PeriodStart != "" && c.PeriodEnd != "" {
			findings = []Finding{{Label: "estimate", Text: "Estimated operating cost: " + c.Amount + " " + c.Currency + " for " + c.PeriodStart + " to " + c.PeriodEnd + ".", EvidenceRef: c.Evidence}}
		}
	}
	for _, f := range findings {
		allowed := f.Label == "observation" || f.Label == "estimate"
		if in.QuestionID != "high_bill_possible_contributors" && f.Label == "hypothesis" {
			allowed = true
		}
		if !allowed || f.Text == "" || len(f.EvidenceRef) == 0 {
			continue
		}
		valid := true
		for _, ref := range f.EvidenceRef {
			if ref.ID == "" || ref.Kind == "" || ref.Revision <= 0 {
				valid = false
			}
		}
		if valid {
			out.Findings = append(out.Findings, f)
			for _, ref := range f.EvidenceRef {
				if in.QuestionID != weatherTipsFAQ || !slices.Contains(out.EvidenceRefs, ref) {
					out.EvidenceRefs = append(out.EvidenceRefs, ref)
				}
			}
		}
	}
	if len(out.Findings) == 0 && in.QuestionID == weatherTipsFAQ && !in.WeatherUnavailable {
		out.Status, out.Text = "supported", NoWeatherTips
		return out, true
	}
	if len(out.Findings) == 0 {
		out.NextAction = &NextAction{Kind: "adaptive_questions", FlowID: "faq-contributors/v1"}
		return out, true
	}
	if in.QuestionID != weatherTipsFAQ {
		sort.Slice(out.Findings, func(i, j int) bool { return out.Findings[i].Text < out.Findings[j].Text })
	}
	out.Status = "partial"
	switch in.QuestionID {
	case "high_bill_possible_contributors":
		out.Text = "These observations give context for possible energy demand. They do not establish a change in home energy use or a bill cause."
	case "device_operating_cost":
		out.Status = "supported"
		out.Text = "An operating cost estimate is available for this device."
	default:
		out.Text = "These options have supporting information. Review their details before making a change."
	}
	return out, true
}
