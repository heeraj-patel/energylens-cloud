package recommendation

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/weather"
	"log/slog"
	"slices"
	"strings"
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
	ordered                               bool // the appliance's weather tips are already in final order
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
	if !w.active() || w.ordered {
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
	var savings householdenergy.WeatherSavings
	if s.WeatherSavings != nil && len(tips) > 0 {
		if savings, err = s.WeatherSavings.WeatherDeviceSavings(ctx, owner, home); err != nil {
			return nil, nil, err
		}
	}
	page.HomeTips = homeTips(tips, state)
	out := make([]Appliance, len(appliances))
	for i, a := range appliances {
		w := *state
		w.MeasuredUse = savings.Devices[a.ID].Metered
		w.ordered = true
		a.Weather = &w
		qualified := qualifyingWeatherTips(a, tips, state, savings)
		a.ResearchTips = append(qualified[:min(len(qualified), maxDeviceWeatherTips)], withoutWeatherTips(a.ResearchTips)...)
		out[i] = a
	}
	return out, page, nil
}

// ActiveWeatherTip is a weather tip that applies now, with the weather
// evidence behind its trigger.
type ActiveWeatherTip struct {
	Fact    datawizfacts.Fact
	Trigger string
	Refs    []Evidence
}

const maxRunEvidenceDays = 14

// ActiveWeather returns the weather block and every weather tip whose trigger
// is active. A snapshot failure reports weather unavailable, never an error.
func (s Service) ActiveWeather(ctx context.Context, owner, home string, now time.Time) (*WeatherPage, []ActiveWeatherTip, error) {
	if s.Weather == nil {
		return unavailablePage(weather.ReasonProviderUnavailable), nil, nil
	}
	snap, err := s.Weather.Snapshot(ctx, owner, home, now)
	if err != nil {
		slog.Error("weather snapshot failed", "error", err)
		return unavailablePage(weather.ReasonProviderUnavailable), nil, nil
	}
	page, state := weatherView(snap)
	if !state.active() {
		return page, nil, nil
	}
	store, ok := s.Store.(weatherTipStore)
	if !ok {
		return page, nil, nil
	}
	tips, err := store.WeatherTips(ctx)
	if err != nil {
		return nil, nil, err
	}
	refs := map[string][]Evidence{
		triggerHeatAlert: alertRefs(snap, weather.KindHot), triggerColdAlert: alertRefs(snap, weather.KindCold),
		triggerHotRun:  runRefs(snap, snap.Hot, func(d weather.StoredDay) bool { return d.HighF != nil }),
		triggerColdRun: runRefs(snap, snap.Cold, func(d weather.StoredDay) bool { return d.LowF != nil }),
	}
	deviceTips, err := s.qualifiedDeviceTips(ctx, owner, home, now, state, tips)
	if err != nil {
		return nil, nil, err
	}
	var out []ActiveWeatherTip
	seen := map[string]bool{}
	for _, tip := range tips {
		v, ok := parseTip(tip)
		if !ok || seen[tip.ID] {
			continue
		}
		if isDeviceWeatherTip(v) {
			if !deviceTips[tip.ID] || seen[tipDedupeKey(tip)] {
				continue
			}
			seen[tipDedupeKey(tip)] = true
		}
		if trigger := state.trigger(v.Context); trigger != "" && len(refs[trigger]) > 0 {
			seen[tip.ID] = true
			out = append(out, ActiveWeatherTip{Fact: tip, Trigger: trigger, Refs: refs[trigger]})
		}
	}
	slices.SortStableFunc(out, func(a, b ActiveWeatherTip) int { return triggerRank(a.Trigger) - triggerRank(b.Trigger) })
	return page, out, nil
}

// isDeviceWeatherTip is a weather tip aimed at appliance categories; tips for
// the home itself, or for no category, are not.
func isDeviceWeatherTip(v tipValue) bool {
	return isWeatherContext(v.Context) && len(v.Applicable) > 0 && !slices.Contains(v.Applicable, "home")
}

// qualifiedDeviceTips returns the ids of device weather tips that qualify for
// at least one appliance in the home.
func (s Service) qualifiedDeviceTips(ctx context.Context, owner, home string, now time.Time, state *WeatherState, tips []datawizfacts.Fact) (map[string]bool, error) {
	out := map[string]bool{}
	hasDevice := false
	for _, tip := range tips {
		if v, ok := parseTip(tip); ok && isDeviceWeatherTip(v) {
			hasDevice = true
		}
	}
	if !hasDevice {
		return out, nil
	}
	appliances, err := s.Store.Current(ctx, owner, home, now)
	if err != nil {
		return nil, err
	}
	var savings householdenergy.WeatherSavings
	if s.WeatherSavings != nil {
		if savings, err = s.WeatherSavings.WeatherDeviceSavings(ctx, owner, home); err != nil {
			return nil, err
		}
	}
	for _, a := range appliances {
		for _, tip := range qualifyingWeatherTips(a, tips, state, savings) {
			out[tip.ID] = true
		}
	}
	return out, nil
}

func alertRefs(snap weather.Snapshot, kind string) []Evidence {
	var out []Evidence
	for _, alert := range snap.Alerts {
		if alert.Kind == kind {
			out = append(out, Evidence{ID: alert.ID, Kind: "weather_alert", Revision: 1})
		}
	}
	return out
}

// runRefs cites the newest days of an active run, at most maxRunEvidenceDays.
func runRefs(snap weather.Snapshot, run weather.Run, present func(weather.StoredDay) bool) []Evidence {
	if !run.Active {
		return nil
	}
	var days []weather.StoredDay
	for _, d := range snap.Days {
		if d.Source == weather.SourceObserved && d.Date <= run.LastDay && present(d) {
			days = append(days, d)
		}
	}
	slices.SortFunc(days, func(a, b weather.StoredDay) int { return strings.Compare(b.Date, a.Date) })
	out := []Evidence{}
	for _, d := range days[:min(len(days), run.Days, maxRunEvidenceDays)] {
		out = append(out, Evidence{ID: d.ID, Kind: "weather_day", Revision: int64(d.Revision)})
	}
	return out
}
