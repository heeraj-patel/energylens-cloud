package democost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const maxDailyCostMicros int64 = 1_000_000_000_000

type Snapshot struct {
	Version             int         `json:"version"`
	FixtureRevision     string      `json:"fixture_revision"`
	Timezone            string      `json:"timezone"`
	ReferenceLocalDate  string      `json:"reference_local_date"`
	AdjustmentLocalDate string      `json:"adjustment_local_date,omitempty"`
	Currency            string      `json:"currency"`
	Provenance          string      `json:"provenance"`
	Devices             []Device    `json:"devices"`
	Unattributed        Attribution `json:"unattributed"`
}
type Costs struct {
	Useful int64 `json:"useful_cost_micros"`
	Waste  int64 `json:"waste_cost_micros"`
}
type Device struct {
	ID        string  `json:"id"`
	Name      *string `json:"name"`
	Reference Costs   `json:"reference"`
	Default   Costs   `json:"default"`
}
type Attribution struct {
	Reference  Costs  `json:"reference"`
	Default    Costs  `json:"default"`
	Adjustment *Costs `json:"adjustment,omitempty"`
}

// Retarget preserves the local schedule and cost amounts while changing the
// timezone used to project each local day. Invalid stored snapshots are cleared
// by the caller instead of being carried into the new home timezone.
func Retarget(raw []byte, oldTimezone, newTimezone string) []byte {
	var s Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil || decoder.Decode(new(any)) != io.EOF || s.Validate() != nil || s.FixtureRevision != "mobile_demo_cost_v1" || s.Timezone != oldTimezone {
		return nil
	}
	s.Timezone = newTimezone
	if s.Validate() != nil {
		return nil
	}
	updated, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return updated
}

func (s Snapshot) Validate() error {
	if s.Version != 1 || s.FixtureRevision == "" || s.Provenance != "synthetic" || s.Currency != "USD" || s.Timezone == "" || len(s.Devices) != 18 {
		return errors.New("snapshot requires version 1, synthetic USD provenance, timezone, revision, and exactly 18 devices")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("invalid snapshot timezone: %w", err)
	}
	if _, err := time.Parse("2006-01-02", s.ReferenceLocalDate); err != nil {
		return errors.New("invalid reference_local_date")
	}
	if s.AdjustmentLocalDate != "" {
		if _, err := time.Parse("2006-01-02", s.AdjustmentLocalDate); err != nil {
			return errors.New("invalid adjustment_local_date")
		}
		if s.Unattributed.Adjustment == nil {
			return errors.New("adjustment_local_date requires unattributed.adjustment")
		}
	}
	if s.Unattributed.Adjustment != nil && s.AdjustmentLocalDate == "" {
		return errors.New("unattributed.adjustment requires adjustment_local_date")
	}
	expectedNames := []*string{stringPtr("Study Desktop & Rig"), stringPtr(`Living Room 65" TV`), stringPtr("Phone & Tablet Chargers"), stringPtr("Kitchen Refrigerator"), stringPtr("Hallway Central AC"), stringPtr("Living Room Soundbar"), stringPtr("Home Office Monitors"), stringPtr("Mesh Wi-Fi Gateway & Router"), stringPtr("Subwoofer & Amp Rack"), stringPtr("Central Heat Pump"), nil, nil, nil, nil, nil, nil, nil, nil}
	for i, d := range s.Devices {
		if d.ID != fmt.Sprintf("demo-device-%02d", i+1) {
			return fmt.Errorf("device %d must be demo-device-%02d", i+1, i+1)
		}
		if (d.Name == nil) != (expectedNames[i] == nil) || d.Name != nil && *d.Name != *expectedNames[i] {
			return fmt.Errorf("device %s has unexpected name", d.ID)
		}
		if !validCosts(d.Reference) || !validCosts(d.Default) {
			return fmt.Errorf("device %s costs must be between zero and the supported maximum", d.ID)
		}
	}
	for _, c := range []Costs{s.Unattributed.Reference, s.Unattributed.Default} {
		if !validCosts(c) {
			return errors.New("unattributed costs exceed the supported range")
		}
	}
	if s.Unattributed.Adjustment != nil && !validCosts(*s.Unattributed.Adjustment) {
		return errors.New("adjustment costs exceed the supported range")
	}
	return nil
}

func validCosts(c Costs) bool {
	return c.Useful >= 0 && c.Useful <= maxDailyCostMicros && c.Waste >= 0 && c.Waste <= maxDailyCostMicros
}
func stringPtr(v string) *string { return &v }
