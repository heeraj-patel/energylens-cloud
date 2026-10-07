package meteredsummary

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"energylens.local/cloud/internal/samplehousehold"
)

// SampleProvider is installed only by the explicit dev/test household gate.
// The route still authenticates, verifies ownership and checks persisted evidence.
type SampleProvider struct {
	AllowedUsers map[string]bool
	AllowedHomes map[string]bool
	row          json.RawMessage
	bucket       time.Time
}

func NewSampleProvider(users, homes map[string]bool) (*SampleProvider, error) {
	if err := samplehousehold.ValidateRelationships(); err != nil {
		return nil, fmt.Errorf("validate metered sample relationships: %w", err)
	}
	var fixture struct {
		FixtureID string `json:"fixture_id"`
		Responses struct {
			Summaries struct {
				Granularity string            `json:"granularity"`
				Rows        []json.RawMessage `json:"summaries"`
			} `json:"metered_summaries"`
		} `json:"domain_api_responses"`
	}
	if err := json.Unmarshal(samplehousehold.Fixture, &fixture); err != nil {
		return nil, fmt.Errorf("decode metered sample: %w", err)
	}
	if fixture.FixtureID != samplehousehold.Revision || fixture.Responses.Summaries.Granularity != "day" || len(fixture.Responses.Summaries.Rows) != 1 {
		return nil, errors.New("invalid metered sample revision or rows")
	}
	row := fixture.Responses.Summaries.Rows[0]
	var fields struct {
		Kind             string            `json:"kind"`
		HubID            string            `json:"hub_id"`
		SensorID         string            `json:"sensor_id"`
		AttachmentID     string            `json:"attachment_id"`
		Status           string            `json:"status"`
		AllocationMethod string            `json:"allocation_method"`
		BucketStart      time.Time         `json:"bucket_start"`
		ComputedAt       time.Time         `json:"computed_at"`
		OwnershipEpoch   int               `json:"ownership_epoch"`
		IntervalCount    int               `json:"interval_count"`
		EnergyWh         *float64          `json:"energy_wh"`
		CoveredSeconds   *float64          `json:"covered_seconds"`
		SourceRefs       []json.RawMessage `json:"source_refs"`
	}
	if err := json.Unmarshal(row, &fields); err != nil || fields.Kind != "energy" || !uuidOK(fields.HubID) || !uuidOK(fields.SensorID) || !uuidOK(fields.AttachmentID) || fields.OwnershipEpoch < 1 || fields.BucketStart.IsZero() || fields.ComputedAt.IsZero() || fields.Status != "no_intervals" || fields.AllocationMethod != "none" || fields.IntervalCount != 0 || fields.EnergyWh != nil || fields.CoveredSeconds != nil || fields.SourceRefs == nil || len(fields.SourceRefs) != 0 {
		return nil, errors.New("invalid metered sample row")
	}
	return &SampleProvider{AllowedUsers: users, AllowedHomes: homes, row: append(json.RawMessage(nil), row...), bucket: fields.BucketStart}, nil
}

func (s *SampleProvider) allowed(q Query) bool {
	return s != nil && s.AllowedUsers[q.OwnerID] && s.AllowedHomes[q.HomeID]
}

func (s *SampleProvider) page(q Query) []json.RawMessage {
	rows := make([]json.RawMessage, 0, 1)
	if q.Granularity == "day" && !s.bucket.Before(q.From) && s.bucket.Before(q.To) {
		rows = append(rows, s.row)
	}
	return rows
}

func (s *SampleProvider) evidenceRevision() string {
	// A content digest changes with the fixture and cannot collide with the
	// real evidence revision's v1-<decimal> namespace.
	digest := sha256.Sum256(samplehousehold.Fixture)
	return fmt.Sprintf("sample-%x", digest[:16])
}

func sampleHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-EnergyLens-Data-Mode", "sample")
	w.Header().Set("X-EnergyLens-Fixture-Revision", samplehousehold.Revision)
}
