package recommendation

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/weather"
	"log/slog"
	"slices"
	"time"
)

const (
	CapabilityWeatherTips = "weather_tips_v1"

	triggerHeatAlert = "heat_alert"
	triggerColdAlert = "cold_alert"
	triggerHotRun    = "hot_run"
	triggerColdRun   = "cold_run"

	maxHomeWeatherTips = 3
	ecccAttribution    = "Data Source: Environment and Climate Change Canada"
)

// WeatherState is a home's active weather triggers for one evaluation. A nil
// state (every capability but weather_tips_v1) attaches no weather tips.
type WeatherState struct {
	HeatAlert, ColdAlert, HotRun, ColdRun bool
	MeasuredUse                           bool
	TOU                                   *householdenergy.TOUDay
}

func (w *WeatherState) active() bool {
	return w != nil && (w.HeatAlert || w.ColdAlert || w.HotRun || w.ColdRun)
}

// trigger names why a weather context applies now: an alert first, else a run.
func (w *WeatherState) trigger(context string) string {
	if w == nil {
		return ""
	}
	switch context {
	case "weather_hot":
		if w.HeatAlert {
			return triggerHeatAlert
		}
		if w.HotRun {
			return triggerHotRun
		}
	case "weather_cold":
		if w.ColdAlert {
			return triggerColdAlert
		}
		if w.ColdRun {
			return triggerColdRun
		}
	}
	return ""
}

func triggerRank(trigger string) int {
	switch trigger {
	case triggerHeatAlert:
		return 0
	case triggerColdAlert:
		return 1
	case triggerHotRun:
		return 2
	case triggerColdRun:
		return 3
	}
	return 4
}

func isWeatherContext(context string) bool {
	return context == "weather_hot" || context == "weather_cold"
}

// WeatherContext is the device-level weather detail; TOU fields appear only on
// shift tips for a home whose tariff has TOU periods that day.
type WeatherContext struct {
	Trigger         string `json:"trigger"`
	MeasuredUse     bool   `json:"measured_use"`
	TOUSavingPerKWh string `json:"tou_saving_per_kwh,omitempty"`
	Currency        string `json:"currency,omitempty"`
	PeakStartLocal  string `json:"peak_start_local,omitempty"`
	PeakEndLocal    string `json:"peak_end_local,omitempty"`
}

type tipValue struct {
	Context     string   `json:"context"`
	Applicable  []string `json:"applicable_categories"`
	Excluded    []string `json:"excluded_categories"`
	Fuels       []string `json:"applicable_fuels"`
	SavingsType string   `json:"savings_type"`
}

func parseTip(tip datawizfacts.Fact) (tipValue, bool) {
	var v tipValue
	return v, tip.Field == "efficiency_tip" && tip.ID != "" && json.Unmarshal(tip.Value, &v) == nil
}

// weatherFirst moves active weather tips ahead of the rest, alert-backed first;
// every other tip keeps its order.
func weatherFirst(tips []datawizfacts.Fact, w *WeatherState) []datawizfacts.Fact {
	if !w.active() {
		return tips
	}
	out := slices.Clone(tips)
	rank := func(tip datawizfacts.Fact) int {
		if v, ok := parseTip(tip); ok {
			return triggerRank(w.trigger(v.Context))
		}
		return triggerRank("")
	}
	slices.SortStableFunc(out, func(a, b datawizfacts.Fact) int { return rank(a) - rank(b) })
	return out
}

func (w *WeatherState) contextFor(trigger string, shift bool, prior *WeatherContext) *WeatherContext {
	wc := prior
	if wc == nil {
		wc = &WeatherContext{Trigger: trigger, MeasuredUse: w.MeasuredUse}
	}
	if shift && w.TOU != nil && wc.TOUSavingPerKWh == "" {
		wc.TOUSavingPerKWh = w.TOU.SavingPerKWh.FloatString(6)
		wc.Currency, wc.PeakStartLocal, wc.PeakEndLocal = w.TOU.Currency, w.TOU.PeakStart, w.TOU.PeakEnd
	}
	return wc
}

type WeatherAlertOut struct {
	Event       string   `json:"event"`
	Headline    string   `json:"headline,omitempty"`
	StartsAt    string   `json:"starts_at"`
	EndsAt      string   `json:"ends_at"`
	SourceURL   string   `json:"source_url"`
	EvidenceRef Evidence `json:"evidence_ref"`
}

type WeatherRunOut struct {
	Kind    string `json:"kind"`
	Days    int    `json:"days"`
	LastDay string `json:"last_day"`
}

type WeatherHomeTip struct {
	Fact    datawizfacts.Fact `json:"fact"`
	Trigger string            `json:"trigger"`
}

// WeatherPage is the response's weather block.
type WeatherPage struct {
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	Provider    string            `json:"provider,omitempty"`
	Attribution string            `json:"attribution,omitempty"`
	Alerts      []WeatherAlertOut `json:"alerts"`
	Runs        []WeatherRunOut   `json:"runs"`
	HomeTips    []WeatherHomeTip  `json:"home_tips"`
}

func unavailablePage(reason string) *WeatherPage {
	return &WeatherPage{Status: weather.StatusUnavailable, Reason: reason, Alerts: []WeatherAlertOut{}, Runs: []WeatherRunOut{}, HomeTips: []WeatherHomeTip{}}
}

// weatherView turns a snapshot into the page block and the active triggers;
// only hot and cold alerts are listed.
func weatherView(snap weather.Snapshot) (*WeatherPage, *WeatherState) {
	if snap.Status != weather.StatusAvailable {
		return unavailablePage(snap.Reason), nil
	}
	page := &WeatherPage{Status: snap.Status, Provider: snap.Provider, Alerts: []WeatherAlertOut{}, Runs: []WeatherRunOut{}, HomeTips: []WeatherHomeTip{}}
	if snap.Provider == weather.ProviderECCC {
		page.Attribution = ecccAttribution
	}
	state := &WeatherState{HotRun: snap.Hot.Active, ColdRun: snap.Cold.Active}
	for _, alert := range snap.Alerts {
		if alert.Kind == "" {
			continue
		}
		state.HeatAlert = state.HeatAlert || alert.Kind == weather.KindHot
		state.ColdAlert = state.ColdAlert || alert.Kind == weather.KindCold
		out := WeatherAlertOut{Event: alert.Event, StartsAt: alert.StartsAt.UTC().Format(time.RFC3339), EndsAt: alert.EndsAt.UTC().Format(time.RFC3339),
			SourceURL: alert.SourceURL, EvidenceRef: Evidence{ID: alert.ID, Kind: "weather_alert", Revision: 1}}
		if alert.Headline != nil {
			out.Headline = *alert.Headline
		}
		page.Alerts = append(page.Alerts, out)
	}
	for _, run := range []struct {
		kind string
		r    weather.Run
	}{{weather.KindHot, snap.Hot}, {weather.KindCold, snap.Cold}} {
		if run.r.Active {
			page.Runs = append(page.Runs, WeatherRunOut{Kind: run.kind, Days: run.r.Days, LastDay: run.r.LastDay})
		}
	}
	return page, state
}

// homeTips picks up to three tips tagged for the home itself, alert-backed first.
func homeTips(tips []datawizfacts.Fact, w *WeatherState) []WeatherHomeTip {
	out := []WeatherHomeTip{}
	seen := map[string]bool{}
	for _, tip := range tips {
		v, ok := parseTip(tip)
		if !ok || seen[tip.ID] || !slices.Contains(v.Applicable, "home") {
			continue
		}
		if trigger := w.trigger(v.Context); trigger != "" {
			seen[tip.ID] = true
			out = append(out, WeatherHomeTip{Fact: tip, Trigger: trigger})
		}
	}
	slices.SortStableFunc(out, func(a, b WeatherHomeTip) int { return triggerRank(a.Trigger) - triggerRank(b.Trigger) })
	if len(out) > maxHomeWeatherTips {
		out = out[:maxHomeWeatherTips]
	}
	return out
}

// weatherTipStore is implemented by stores that hold the weather_hot and weather_cold tips.
type weatherTipStore interface {
	WeatherTips(context.Context) ([]datawizfacts.Fact, error)
}

// withWeather fetches the home's weather and returns the appliances carrying
// the active triggers and weather tips. A snapshot failure leaves them
// untouched and reports weather unavailable; other read failures are errors.
func (s Service) withWeather(ctx context.Context, owner, home string, now time.Time, appliances []Appliance) ([]Appliance, *WeatherPage, error) {
	if s.Weather == nil {
		return appliances, unavailablePage(weather.ReasonProviderUnavailable), nil
	}
	snap, err := s.Weather.Snapshot(ctx, owner, home, now)
	if err != nil {
		slog.Error("weather snapshot failed", "error", err)
		return appliances, unavailablePage(weather.ReasonProviderUnavailable), nil
	}
	page, state := weatherView(snap)
	if !state.active() {
		return appliances, page, nil
	}
	var tips []datawizfacts.Fact
	if store, ok := s.Store.(weatherTipStore); ok {
		if tips, err = store.WeatherTips(ctx); err != nil {
			return nil, nil, err
		}
	}
	if s.TOU != nil {
		if state.TOU, err = s.TOU.TOUDay(ctx, owner, home, now); err != nil {
			return nil, nil, err
		}
	}
	measured := map[string]bool{}
	if s.Waste != nil {
		waste, err := s.Waste.DeviceMonthlyWaste(ctx, owner, home)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range waste {
			measured[item.ApplianceID] = true
		}
	}
	page.HomeTips = homeTips(tips, state)
	out := make([]Appliance, len(appliances))
	for i, a := range appliances {
		w := *state
		w.MeasuredUse = measured[a.ID]
		a.Weather = &w
		a.ResearchTips = append(slices.Clone(a.ResearchTips), tips...)
		out[i] = a
	}
	return out, page, nil
}
