package faq

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestRecentUseFindingsRejectStaleWrongPurposeAndUnknown(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	base := RecentUse{ID: uuid.NewString(), ApplianceID: uuid.NewString(), Category: "washer", QuestionID: "faq-contributors/v1.washer.recent_cycles", Status: "confirmed", ApplianceVersion: 2, QuestionVersion: 1, Revision: 3, Value: json.RawMessage("0"), ConfirmedAt: now.Add(-time.Hour), RecordedAt: now.Add(-time.Hour)}
	findings, expiry := RecentUseFindings([]RecentUse{base}, now)
	if len(findings) != 1 || !strings.Contains(findings[0].Text, "0 cycles") || len(findings[0].EvidenceRef) != 2 || !expiry.Equal(base.ConfirmedAt.Add(24*time.Hour)) {
		t.Fatalf("confirmed zero: %+v %v", findings, expiry)
	}
	cases := []RecentUse{base, base, base, base, base}
	cases[0].ConfirmedAt = now.Add(-25 * time.Hour)
	cases[1].Status = "unknown"
	cases[2].QuestionID = "washer.cycles_per_week"
	cases[3].ApplianceVersion = 0
	cases[4].Value = json.RawMessage("1.5")
	for i, a := range cases {
		if f, _ := RecentUseFindings([]RecentUse{a}, now); len(f) != 0 {
			t.Fatalf("invalid %d: %+v", i, f)
		}
	}
}
