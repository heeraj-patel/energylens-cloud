package inventory

import (
	"encoding/json"
	"errors"
	"fmt"

	"energylens.local/cloud/internal/samplehousehold"
)

const SampleRevision = samplehousehold.Revision

// SampleProvider is installed only under the dev/test household sample gate.
// Authentication, ownership and persisted inventory are checked by the route.
type SampleProvider struct {
	AllowedUsers map[string]bool
	AllowedHomes map[string]bool
	appliances   []Appliance
}

func NewSampleProvider(users, homes map[string]bool) (*SampleProvider, error) {
	if err := samplehousehold.ValidateRelationships(); err != nil {
		return nil, fmt.Errorf("validate appliance sample relationships: %w", err)
	}
	var fixture struct {
		FixtureID          string `json:"fixture_id"`
		DomainAPIResponses struct {
			Appliances struct {
				Appliances []Appliance `json:"appliances"`
			} `json:"appliances"`
		} `json:"domain_api_responses"`
	}
	if err := json.Unmarshal(samplehousehold.Fixture, &fixture); err != nil {
		return nil, fmt.Errorf("decode appliance sample: %w", err)
	}
	items := fixture.DomainAPIResponses.Appliances.Appliances
	if fixture.FixtureID != SampleRevision || len(items) != 4 {
		return nil, errors.New("invalid appliance sample revision or row count")
	}
	for _, a := range items {
		if !validUUID(a.ID) || !validUUID(a.HomeID) || a.Version != 1 || a.ArchivedAt != nil || a.Identity.MatchQuality != "unresolved" {
			return nil, errors.New("invalid appliance sample row")
		}
	}
	return &SampleProvider{AllowedUsers: users, AllowedHomes: homes, appliances: items}, nil
}

func (s *SampleProvider) Appliances(home string) []Appliance {
	out := make([]Appliance, len(s.appliances))
	copy(out, s.appliances)
	for i := range out {
		out[i].HomeID = home
	}
	return out
}

func (s *SampleProvider) Appliance(home, id string) (Appliance, bool) {
	for _, a := range s.appliances {
		if a.ID == id {
			a.HomeID = home
			return a, true
		}
	}
	return Appliance{}, false
}
