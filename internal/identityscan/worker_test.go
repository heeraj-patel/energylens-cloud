package identityscan

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCandidateReportValidation(t *testing.T) {
	r := Request{Category: "dryer", Manufacturer: "Acme", Model: "A1"}
	for _, tc := range []struct{ body, state string }{
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"identity_review_candidate"}]}`, "review_pending"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence"}]}`, "missing_source"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"rated_input_only"}]}`, "missing_source"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"annual_manifest_present"}]}`, "reviewed_present"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence","waste_evidence":{"status":"not_found","checked_sources":["epa","configured_site_search"],"search":{"status":"no_search_leads"}}}]}`, "missing_source"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence","waste_evidence":{"status":"review_candidate"}}]}`, "review_pending"},
		{`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence","waste_evidence":{"status":"review_required"}}]}`, "review_pending"},
	} {
		state, err := status(json.RawMessage(tc.body), r)
		if err != nil || state != tc.state {
			t.Fatalf("status %q: %q %v", tc.state, state, err)
		}
	}
	for _, body := range []string{`{}`, `{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Other","model":"A1","status":"identity_review_candidate"}]}`, `{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"supported"}]}`} {
		if _, err := status(json.RawMessage(body), r); err == nil {
			t.Fatalf("accepted report %s", body)
		}
	}
	if _, err := status(json.RawMessage(`{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence","epa_check":{"status":"lookup_error"}}]}`), r); err == nil {
		t.Fatal("provider failure mislabeled missing source")
	}
	for _, waste := range []string{
		`{"status":"lookup_error"}`,
		`{"status":"not_found"}`,
		`{"status":"not_found","checked_sources":["epa","configured_site_search"],"search":{"status":"search_leads"}}`,
	} {
		body := `{"schema_version":1,"model_count":1,"models":[{"category":"dryer","manufacturer":"Acme","model":"A1","status":"no_evidence","waste_evidence":` + waste + `}]}`
		if _, err := status(json.RawMessage(body), r); err == nil {
			t.Fatalf("accepted incomplete or failed waste check: %s", waste)
		}
	}
}

type fakeScanner struct {
	calls  int
	report json.RawMessage
}

func (s *fakeScanner) Scan(_ context.Context, _ Request) (json.RawMessage, error) {
	s.calls++
	if s.report == nil {
		return nil, errors.New("offline")
	}
	return s.report, nil
}

func TestRunLimitRejectsUnboundedPass(t *testing.T) {
	s := Store{}
	for _, limit := range []int{0, 11, 1000} {
		if _, err := s.Run(context.Background(), &fakeScanner{}, false, limit, time.Now()); err == nil {
			t.Fatalf("accepted batch %d", limit)
		}
	}
}

func TestKBStdoutIsBoundedWhileWriting(t *testing.T) {
	b := cappedBuffer{Max: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil || b.Len() != 4 {
		t.Fatalf("unbounded KB output: len=%d err=%v", b.Len(), err)
	}
}
