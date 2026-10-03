package faq

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RecentUse is the latest response to the FAQ-specific seven-day prompt for
// one current appliance identity. Unknown responses supersede older answers.
type RecentUse struct {
	ID, ApplianceID, Category, QuestionID, Status string
	ApplianceVersion, QuestionVersion, Revision   int64
	Value                                         json.RawMessage
	ConfirmedAt, RecordedAt                       time.Time
}

var recentQuestion = map[string]string{
	"washer": "faq-contributors/v1.washer.recent_cycles", "dryer": "faq-contributors/v1.dryer.recent_cycles",
	"dishwasher": "faq-contributors/v1.dishwasher.recent_cycles", "computer": "faq-contributors/v1.computer.recent_hours",
	"television": "faq-contributors/v1.television.recent_hours", "space_heater": "faq-contributors/v1.space_heater.recent_hours",
	"air_conditioner": "faq-contributors/v1.air_conditioner.recent_hours",
}

// RecentUseFindings state only what the customer reported. A response is
// current for one day after confirmation; it does not prove measured use or
// establish a change from a previous week.
func RecentUseFindings(answers []RecentUse, now time.Time) ([]Finding, time.Time) {
	var findings []Finding
	var expiry time.Time
	for _, a := range answers {
		expected, ok := recentQuestion[a.Category]
		if !ok || a.QuestionID != expected || a.QuestionVersion != 1 || a.ApplianceVersion < 1 || a.Revision < 1 || a.Status != "confirmed" || a.ConfirmedAt.IsZero() || a.ConfirmedAt.After(now) || a.RecordedAt.After(now) || a.RecordedAt.Before(a.ConfirmedAt.Add(-5*time.Minute)) || now.Sub(a.ConfirmedAt) >= 24*time.Hour {
			continue
		}
		if _, err := uuid.Parse(a.ID); err != nil {
			continue
		}
		if _, err := uuid.Parse(a.ApplianceID); err != nil {
			continue
		}
		var number json.Number
		decoder := json.NewDecoder(strings.NewReader(string(a.Value)))
		decoder.UseNumber()
		if err := decoder.Decode(&number); err != nil {
			continue
		}
		value, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 168 {
			continue
		}
		unit := "hours"
		if strings.HasSuffix(a.QuestionID, "recent_cycles") {
			if value > 100 || math.Trunc(value) != value {
				continue
			}
			unit = "cycles"
		}
		until := a.ConfirmedAt.Add(24 * time.Hour)
		findings = append(findings, Finding{Label: "observation", Text: fmt.Sprintf("Customer-reported recent use: %s had %s %s in the seven days before %s. This is self-reported context, not a measured change or a bill cause. Answer expires %s.", strings.ReplaceAll(a.Category, "_", " "), number.String(), unit, a.ConfirmedAt.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)), EvidenceRef: []Evidence{{ID: a.ApplianceID, Kind: "appliance", Revision: a.ApplianceVersion}, {ID: a.ID, Kind: "question_answer", Revision: a.Revision}}})
		if expiry.IsZero() || until.Before(expiry) {
			expiry = until
		}
	}
	return findings, expiry
}
