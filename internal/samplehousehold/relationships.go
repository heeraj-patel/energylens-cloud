package samplehousehold

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// ValidateRelationships checks the internal links shared by the two sample
// routes. The synthetic projection IDs are separate from these API UUIDs.
func ValidateRelationships() error {
	return validateRelationships(Fixture)
}

func validateRelationships(data []byte) error {
	var fixture struct {
		FixtureID     string `json:"fixture_id"`
		Relationships struct {
			HomeID      string `json:"home_id"`
			Attachments []struct {
				ID             string `json:"id"`
				ApplianceID    string `json:"appliance_id"`
				HomeID         string `json:"home_id"`
				HubID          string `json:"hub_id"`
				SensorID       string `json:"sensor_id"`
				OwnershipEpoch int    `json:"ownership_epoch"`
			} `json:"attachments"`
		} `json:"domain_api_relationships"`
		Responses struct {
			Appliances struct {
				Rows []struct {
					ID     string `json:"id"`
					HomeID string `json:"home_id"`
				} `json:"appliances"`
			} `json:"appliances"`
			Summaries struct {
				Rows []struct {
					AttachmentID     string            `json:"attachment_id"`
					HubID            string            `json:"hub_id"`
					SensorID         string            `json:"sensor_id"`
					OwnershipEpoch   int               `json:"ownership_epoch"`
					Status           string            `json:"status"`
					AllocationMethod string            `json:"allocation_method"`
					IntervalCount    int               `json:"interval_count"`
					EnergyWh         *float64          `json:"energy_wh"`
					CoveredSeconds   *float64          `json:"covered_seconds"`
					SourceRefs       []json.RawMessage `json:"source_refs"`
				} `json:"summaries"`
			} `json:"metered_summaries"`
		} `json:"domain_api_responses"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	validID := func(id string) bool {
		parsed, err := uuid.Parse(id)
		return err == nil && parsed.String() == id
	}
	home := fixture.Relationships.HomeID
	if fixture.FixtureID != Revision || !validID(home) || len(fixture.Responses.Appliances.Rows) != 4 || len(fixture.Relationships.Attachments) != 1 || len(fixture.Responses.Summaries.Rows) != 1 {
		return errors.New("invalid sample relationships")
	}
	appliances := make(map[string]bool, 4)
	for _, row := range fixture.Responses.Appliances.Rows {
		if !validID(row.ID) || row.HomeID != home || appliances[row.ID] {
			return errors.New("invalid sample appliance relationship")
		}
		appliances[row.ID] = true
	}
	link := fixture.Relationships.Attachments[0]
	row := fixture.Responses.Summaries.Rows[0]
	if !validID(link.ID) || !appliances[link.ApplianceID] || link.HomeID != home || !validID(link.HubID) || !validID(link.SensorID) || link.OwnershipEpoch < 1 || row.AttachmentID != link.ID || row.HubID != link.HubID || row.SensorID != link.SensorID || row.OwnershipEpoch != link.OwnershipEpoch {
		return errors.New("invalid sample metered relationship")
	}
	if row.Status != "no_intervals" || row.AllocationMethod != "none" || row.IntervalCount != 0 || row.EnergyWh != nil || row.CoveredSeconds != nil || row.SourceRefs == nil || len(row.SourceRefs) != 0 {
		return errors.New("sample metered energy must remain unknown")
	}
	return nil
}
