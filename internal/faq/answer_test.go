package faq

import (
	"testing"
	"time"
)

func TestInsufficientHighBillAnswerIsExact(t *testing.T) {
	got, ok := Render(Input{QuestionID: "high_bill_possible_contributors", AsOf: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if !ok || got.Text != Insufficient || got.Status != "insufficient_information" || len(got.Findings) != 0 || got.NextAction == nil || got.NextAction.FlowID != "faq-contributors/v1" {
		t.Fatalf("fallback: %+v", got)
	}
}

func TestOnlyEvidenceBackedFactsAreRendered(t *testing.T) {
	good := Finding{Label: "observation", Text: "Runtime rose in the covered period.", EvidenceRef: []Evidence{{ID: "reading-1", Kind: "metered_history", Revision: 1}}}
	got, _ := Render(Input{QuestionID: "high_bill_possible_contributors", ObservedPatterns: []Finding{
		{Label: "hypothesis", Text: "A weather change caused the bill.", EvidenceRef: good.EvidenceRef},
		{Label: "observation", Text: "No reference."}, good}, AsOf: time.Now()})
	if got.Status != "partial" || len(got.Findings) != 1 || got.Findings[0].Text != good.Text || len(got.EvidenceRefs) != 1 {
		t.Fatalf("unsupported finding escaped: %+v", got)
	}
}

func TestOperatingCostNeedsSource(t *testing.T) {
	in := Input{QuestionID: "device_operating_cost", OperatingCost: &OperatingCost{Amount: "1.23", Currency: "USD", PeriodStart: "2026-10-01", PeriodEnd: "2026-11-01"}, AsOf: time.Now()}
	got, _ := Render(in)
	if got.Text != Insufficient {
		t.Fatalf("unsourced estimate shown: %+v", got)
	}
	in.OperatingCost.Evidence = []Evidence{{ID: "fact-1", Kind: "catalog_fact", Revision: 1}}
	got, _ = Render(in)
	if got.Status != "supported" || len(got.Findings) != 1 {
		t.Fatalf("sourced estimate missing: %+v", got)
	}
}
