// Package appliancefacts composes one appliance's facts from the existing
// in-process owner reads. Each section fails independently.
package appliancefacts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"energylens.local/cloud/internal/datawizresolution"
	"energylens.local/cloud/internal/estimation"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/questions"
	"energylens.local/cloud/internal/recommendation"
)

const (
	StatusOK          = "ok"
	StatusEmpty       = "empty"
	StatusUnavailable = "unavailable"
	StatusError       = "error"
	maxPrograms       = 100
)

type Section struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type Identity struct {
	Section
	Appliance *inventory.Appliance `json:"appliance,omitempty"`
}
type CatalogFacts struct {
	Section
	Resolution json.RawMessage `json:"resolution,omitempty"`
}
type Estimate struct {
	Section
	Estimate json.RawMessage `json:"estimate,omitempty"`
}
type Efficiency struct {
	Section
	Window      string                                 `json:"window,omitempty"`
	NightWindow *householdenergy.EfficiencyNightWindow `json:"night_window,omitempty"`
	AwaySource  string                                 `json:"away_source,omitempty"`
	Device      *householdenergy.DeviceEfficiency      `json:"device,omitempty"`
}
type Programs struct {
	Section
	Items *[]programs.OwnerProgram `json:"items,omitempty"`
	Total *int                     `json:"total,omitempty"`
}
type Actions struct {
	Section
	Items *[]recommendation.Action `json:"items,omitempty"`
}
type UsageAnswers struct {
	Section
	Items *[]questions.Answer `json:"items,omitempty"`
}

type Facts struct {
	ApplianceID      string       `json:"appliance_id"`
	ApplianceVersion int64        `json:"appliance_version"`
	GeneratedAt      string       `json:"generated_at"`
	Identity         Identity     `json:"identity"`
	CatalogFacts     CatalogFacts `json:"catalog_facts"`
	Estimate         Estimate     `json:"estimate"`
	Efficiency       Efficiency   `json:"efficiency"`
	Programs         Programs     `json:"programs"`
	Tips             Actions      `json:"tips"`
	Care             Actions      `json:"care"`
	UsageAnswers     UsageAnswers `json:"usage_answers"`
}

type ApplianceReader interface {
	Get(ctx context.Context, owner, home, id string) (inventory.Appliance, error)
	ListWithRevision(ctx context.Context, owner, home string) ([]inventory.Appliance, string, error)
}
type ResolutionReader interface {
	ForOwner(ctx context.Context, owner, home, appliance string) (json.RawMessage, error)
}
type EstimateReader interface {
	ListRuns(ctx context.Context, owner, home string, appliance *string, now time.Time) ([]json.RawMessage, error)
}
type AnswerReader interface {
	CurrentAnswers(ctx context.Context, owner, home, appliance string) ([]questions.Answer, error)
}

type EfficiencyReader interface {
	TrailingEfficiency(ctx context.Context, owner, home string) (*householdenergy.MeasuredEfficiency, error)
}

// Revisions fingerprints every input the sections read besides the appliance version.
type Revisions interface {
	Fingerprint(ctx context.Context, home, appliance string, now time.Time) (string, error)
}

type Composer struct {
	Appliances      ApplianceReader
	Resolutions     ResolutionReader
	Estimates       EstimateReader
	Programs        programs.OwnerStore
	Recommendations recommendation.EvidenceStore
	Answers         AnswerReader
	Efficiency      EfficiencyReader
	Revisions       Revisions
}

func (c Composer) compose(ctx context.Context, owner, home string, a inventory.Appliance, capability string, now time.Time) Facts {
	f := Facts{ApplianceID: a.ID, ApplianceVersion: a.Version, GeneratedAt: now.Format(time.RFC3339)}
	f.Identity = Identity{Section{Status: StatusOK}, &a}
	f.CatalogFacts = c.catalogFacts(ctx, owner, home, a.ID)
	f.Estimate = c.estimate(ctx, owner, home, a.ID, now)
	f.Efficiency = c.efficiency(ctx, owner, home, a.ID)
	f.Programs = c.programs(ctx, owner, home, a.ID, now)
	f.Tips, f.Care = c.actions(ctx, owner, home, a.ID, capability, now)
	f.UsageAnswers = c.answers(ctx, owner, home, a.ID)
	return f
}

func failed(what string, err error) Section {
	slog.Error("appliance facts section failed", "section", what, "error", err)
	return Section{Status: StatusError, Reason: what + "_read_failed"}
}

func (c Composer) catalogFacts(ctx context.Context, owner, home, id string) CatalogFacts {
	raw, err := c.Resolutions.ForOwner(ctx, owner, home, id)
	switch {
	case errors.Is(err, datawizresolution.ErrNotFound):
		return CatalogFacts{Section: Section{Status: StatusEmpty, Reason: "resolution_pending"}}
	case err != nil:
		return CatalogFacts{Section: failed("resolution", err)}
	}
	return CatalogFacts{Section{Status: StatusOK}, raw}
}

func (c Composer) estimate(ctx context.Context, owner, home, id string, now time.Time) Estimate {
	runs, err := c.Estimates.ListRuns(ctx, owner, home, &id, now)
	switch {
	case errors.Is(err, estimation.ErrNotFound):
		return Estimate{Section: Section{Status: StatusUnavailable, Reason: "appliance_not_estimable"}}
	case err != nil:
		return Estimate{Section: failed("estimate", err)}
	case len(runs) == 0:
		return Estimate{Section: Section{Status: StatusEmpty, Reason: "no_estimate"}}
	}
	var head struct {
		Kind   string `json:"kind"`
		Reason string `json:"unavailable_reason"`
	}
	if err := json.Unmarshal(runs[0], &head); err != nil {
		return Estimate{Section: failed("estimate", err)}
	}
	if head.Kind == "unavailable" {
		reason := head.Reason
		if reason == "" {
			reason = "estimate_unavailable"
		}
		return Estimate{Section: Section{Status: StatusUnavailable, Reason: reason}}
	}
	return Estimate{Section{Status: StatusOK}, runs[0]}
}

func (c Composer) efficiency(ctx context.Context, owner, home, id string) Efficiency {
	if c.Efficiency == nil {
		return Efficiency{Section: Section{Status: StatusUnavailable, Reason: "measured_efficiency_not_built"}}
	}
	m, err := c.Efficiency.TrailingEfficiency(ctx, owner, home)
	switch {
	case err != nil:
		return Efficiency{Section: failed("efficiency", err)}
	case m == nil:
		return Efficiency{Section: Section{Status: StatusUnavailable, Reason: "measured_efficiency_unavailable"}}
	}
	for i := range m.Devices {
		if m.Devices[i].ApplianceID == id {
			nw := m.NightWindow
			return Efficiency{Section{Status: StatusOK}, "trailing_30_days", &nw, m.AwaySource, &m.Devices[i]}
		}
	}
	return Efficiency{Section: Section{Status: StatusUnavailable, Reason: "appliance_not_in_efficiency"}}
}

func (c Composer) programs(ctx context.Context, owner, home, id string, now time.Time) Programs {
	result, err := c.Programs.List(ctx, owner, home, id, now)
	switch {
	case errors.Is(err, programs.ErrLocationRequired):
		return emptyPrograms("location_required")
	case errors.Is(err, programs.ErrOwnerHomeNotFound):
		return Programs{Section: Section{Status: StatusUnavailable, Reason: "appliance_not_matchable"}}
	case err != nil:
		return Programs{Section: failed("programs", err)}
	}
	if len(result.Programs) == 0 {
		return emptyPrograms("")
	}
	total := len(result.Programs)
	items := result.Programs
	if len(items) > maxPrograms {
		items = items[:maxPrograms]
	}
	return Programs{Section{Status: StatusOK}, &items, &total}
}

func emptyPrograms(reason string) Programs {
	items, total := []programs.OwnerProgram{}, 0
	return Programs{Section{Status: StatusEmpty, Reason: reason}, &items, &total}
}

func (c Composer) actions(ctx context.Context, owner, home, id, capability string, now time.Time) (Actions, Actions) {
	appliances, err := c.Recommendations.Current(ctx, owner, home, now)
	if err != nil {
		s := Actions{Section: failed("recommendation", err)}
		return s, s
	}
	var acts []recommendation.Action
	for _, a := range appliances {
		if a.ID != id {
			continue
		}
		acts = recommendation.EvaluatorFor(capability)(a)
	}
	if lifecycle, ok := c.Recommendations.(recommendation.LifecycleStore); ok {
		events, err := lifecycle.Latest(ctx, owner, home)
		if err != nil {
			s := Actions{Section: failed("recommendation", err)}
			return s, s
		}
		acts = recommendation.ApplyLifecycle(acts, events, now)
	}
	tips, care := []recommendation.Action{}, []recommendation.Action{}
	for _, a := range acts {
		if a.TipCategory == recommendation.TipCare {
			care = append(care, a)
		} else {
			tips = append(tips, a)
		}
	}
	return actionSection(tips), actionSection(care)
}

func actionSection(items []recommendation.Action) Actions {
	if len(items) == 0 {
		return Actions{Section{Status: StatusEmpty}, &items}
	}
	return Actions{Section{Status: StatusOK}, &items}
}

func (c Composer) answers(ctx context.Context, owner, home, id string) UsageAnswers {
	items, err := c.Answers.CurrentAnswers(ctx, owner, home, id)
	switch {
	case errors.Is(err, questions.ErrNotFound):
		return UsageAnswers{Section: Section{Status: StatusUnavailable, Reason: "appliance_not_answerable"}}
	case err != nil:
		return UsageAnswers{Section: failed("usage_answers", err)}
	case len(items) == 0:
		items = []questions.Answer{}
		return UsageAnswers{Section{Status: StatusEmpty}, &items}
	}
	return UsageAnswers{Section{Status: StatusOK}, &items}
}
