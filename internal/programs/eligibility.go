// Package programs evaluates reviewed incentive requirements without treating
// a ZIP match or a provider candidate as proof of eligibility.
package programs

import "time"

type Program struct {
	ID              string
	Status          string // open, upcoming, exhausted, closed
	StartsOn        string // inclusive YYYY-MM-DD
	EndsOn          string // inclusive YYYY-MM-DD
	ZIPs            []string
	Category        string
	Fuel            string
	ProviderID      string // empty for programs without a utility requirement
	RequiresIncome  bool
	RequiresAddress bool
	ReviewedAt      time.Time
	ReviewExpiresAt time.Time
}

type Household struct {
	ZIP               string
	Category          string
	Fuel              string
	ConfirmedProvider map[string]bool
	CandidateProvider map[string]bool
	IncomeConfirmed   bool
	AddressConfirmed  bool
}

type Result struct {
	ProgramID string   `json:"program_id"`
	Status    string   `json:"status"` // potentially_eligible, unknown, ineligible
	Reasons   []string `json:"reasons"`
}

// Evaluate is deliberately conservative. It checks only the modeled terms;
// callers must show the reviewed source and must not present this result as an
// award or guaranteed savings. Dates use the home's local calendar day.
func Evaluate(p Program, h Household, localDay time.Time) Result {
	r := Result{ProgramID: p.ID, Status: "potentially_eligible", Reasons: []string{}}
	if p.ID == "" || p.ReviewedAt.IsZero() || p.Status == "" {
		return Result{ProgramID: p.ID, Status: "unknown", Reasons: []string{"source_unreviewed"}}
	}
	if !p.ReviewExpiresAt.IsZero() && localDay.After(p.ReviewExpiresAt) {
		return Result{ProgramID: p.ID, Status: "unknown", Reasons: []string{"source_review_expired"}}
	}
	for _, date := range []string{p.StartsOn, p.EndsOn} {
		if date == "" {
			continue
		}
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil || parsed.Format("2006-01-02") != date {
			return Result{ProgramID: p.ID, Status: "unknown", Reasons: []string{"invalid_program_date"}}
		}
	}
	if p.Status == "closed" || p.Status == "exhausted" {
		return Result{ProgramID: p.ID, Status: "ineligible", Reasons: []string{"program_not_open"}}
	}
	if p.Status == "upcoming" {
		r.Status = "unknown"
		r.Reasons = append(r.Reasons, "program_upcoming")
	} else if p.Status != "open" {
		r.Status = "unknown"
		r.Reasons = append(r.Reasons, "status_unknown")
	}
	day := localDay.Format("2006-01-02")
	if p.StartsOn != "" && day < p.StartsOn {
		r.Status = "unknown"
		r.Reasons = append(r.Reasons, "not_started")
	}
	if p.EndsOn != "" && day > p.EndsOn {
		return Result{ProgramID: p.ID, Status: "ineligible", Reasons: []string{"expired"}}
	}
	if p.Category != "" && p.Category != h.Category || p.Fuel != "" && p.Fuel != h.Fuel {
		return Result{ProgramID: p.ID, Status: "ineligible", Reasons: []string{"device_mismatch"}}
	}
	if len(p.ZIPs) > 0 {
		matched := false
		for _, zip := range p.ZIPs {
			matched = matched || zip == h.ZIP
		}
		if !matched {
			return Result{ProgramID: p.ID, Status: "ineligible", Reasons: []string{"zip_mismatch"}}
		}
	}
	if p.ProviderID != "" && !h.ConfirmedProvider[p.ProviderID] {
		r.Status = "unknown"
		if h.CandidateProvider[p.ProviderID] {
			r.Reasons = append(r.Reasons, "provider_unconfirmed")
		} else {
			r.Reasons = append(r.Reasons, "provider_unknown")
		}
	}
	if p.RequiresIncome && !h.IncomeConfirmed {
		r.Status = "unknown"
		r.Reasons = append(r.Reasons, "income_unknown")
	}
	if p.RequiresAddress && !h.AddressConfirmed {
		r.Status = "unknown"
		r.Reasons = append(r.Reasons, "address_unknown")
	}
	if r.Status == "potentially_eligible" {
		r.Reasons = append(r.Reasons, "terms_require_confirmation")
	}
	return r
}
