package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"

	ReasonUnsupportedLocation = "unsupported_location"
	ReasonProviderUnavailable = "provider_unavailable"
	ReasonInvalidSourceData   = "invalid_source_data"

	ProviderNWS  = "nws"
	ProviderECCC = "eccc"

	KindHot  = "hot"
	KindCold = "cold"

	SourceForecast = "forecast"

	dateLayout    = "2006-01-02"
	defaultBudget = 4 * time.Second
	runLookback   = 366
)

var (
	ErrProviderUnavailable = errors.New("weather provider unavailable")
	ErrInvalidSourceData   = errors.New("invalid weather source data")
	ErrNoProviderPoint     = errors.New("weather provider has no data for location")
)

// Alert is a stored official alert; Kind is hot, cold or empty when neither.
type Alert struct {
	ID        string
	Provider  string
	Event     string
	Headline  *string
	StartsAt  time.Time
	EndsAt    time.Time
	SourceURL string
	Kind      string
}

// StoredDay is a weather_days row; evidence refs cite ID and Revision.
type StoredDay struct {
	Day
	ID       string
	Revision int
}

// Snapshot is one home's live weather state. Alerts are active in the latest
// alerts fetch; Hot and Cold come from observed days only (Runs).
type Snapshot struct {
	Status        string
	Reason        string
	Provider      string
	LocationKey   string
	Timezone      string
	LastCompleted string
	Alerts        []Alert
	Hot, Cold     Run
	Days          []StoredDay
}

type forecastDay struct {
	Date  string
	HighF *float64
	LowF  *float64
}

type alertRecord struct {
	SourceID  string
	Event     string
	Headline  *string
	StartsAt  time.Time
	EndsAt    time.Time
	SourceURL string
	Kind      string
}

// Live fetches forecasts and alerts live, at most once in flight per
// location, and serves them from weather_locations until their cache expiry.
type Live struct {
	Store  PGLocations
	NWS    NWS
	ECCC   ECCC
	Budget time.Duration

	mu       sync.Mutex
	inflight map[string]*flight
}

// DefaultUserAgent names EnergyLens to NWS and ECCC when none is configured.
const DefaultUserAgent = "EnergyLens (weather tips)"

// NewLive builds the production NWS and ECCC clients.
func NewLive(pool *pgxpool.Pool, userAgent string) *Live {
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return &Live{Store: PGLocations{Pool: pool}, NWS: NWS{UserAgent: userAgent, Client: client}, ECCC: ECCC{UserAgent: userAgent, Client: client}, Budget: defaultBudget}
}

type flight struct {
	done chan struct{}
	err  error
}

// Snapshot returns status unavailable with a reason for unsupported homes and
// provider failures; only store errors and ErrHomeNotFound are returned as errors.
func (s *Live) Snapshot(ctx context.Context, owner, home string, now time.Time) (Snapshot, error) {
	now = now.UTC().Truncate(time.Microsecond)
	loc, supported, err := s.Store.Resolve(ctx, owner, home)
	if err != nil {
		return Snapshot{}, err
	}
	if !supported {
		return unavailable(ReasonUnsupportedLocation), nil
	}
	tz, err := time.LoadLocation(loc.Timezone)
	if err != nil {
		return unavailable(ReasonUnsupportedLocation), nil
	}
	if err := s.refreshOnce(ctx, loc.Key, now); err != nil {
		switch {
		case errors.Is(err, ErrNoProviderPoint):
			return unavailable(ReasonUnsupportedLocation), nil
		case errors.Is(err, ErrInvalidSourceData):
			return unavailable(ReasonInvalidSourceData), nil
		case errors.Is(err, ErrProviderUnavailable):
			return unavailable(ReasonProviderUnavailable), nil
		default:
			return Snapshot{}, err
		}
	}
	local := now.In(tz)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	lastCompleted := today.AddDate(0, 0, -1).Format(dateLayout)
	days, err := s.Store.days(ctx, loc.Key, today.AddDate(0, 0, -runLookback).Format(dateLayout))
	if err != nil {
		return Snapshot{}, err
	}
	alerts, err := s.Store.activeAlerts(ctx, loc.Key, now)
	if err != nil {
		return Snapshot{}, err
	}
	plain := make([]Day, len(days))
	for i, d := range days {
		plain[i] = d.Day
	}
	hot, cold := Runs(plain, lastCompleted)
	return Snapshot{Status: StatusAvailable, Provider: loc.Provider, LocationKey: loc.Key, Timezone: loc.Timezone, LastCompleted: lastCompleted, Alerts: alerts, Hot: hot, Cold: cold, Days: days}, nil
}

func unavailable(reason string) Snapshot {
	return Snapshot{Status: StatusUnavailable, Reason: reason}
}

// refreshOnce joins an in-flight refresh of the same location instead of starting another.
func (s *Live) refreshOnce(ctx context.Context, key string, now time.Time) error {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*flight{}
	}
	if f, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrProviderUnavailable, ctx.Err())
		}
	}
	f := &flight{done: make(chan struct{})}
	s.inflight[key] = f
	s.mu.Unlock()
	budget := s.Budget
	if budget <= 0 {
		budget = defaultBudget
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	f.err = s.refresh(fctx, key, now)
	cancel()
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	close(f.done)
	return f.err
}

func (s *Live) refresh(ctx context.Context, key string, now time.Time) error {
	loc, err := s.Store.location(ctx, key)
	if err != nil {
		return err
	}
	tz, err := time.LoadLocation(loc.Timezone)
	if err != nil {
		return fmt.Errorf("%w: timezone", ErrNoProviderPoint)
	}
	switch loc.Provider {
	case ProviderNWS:
		return s.refreshNWS(ctx, loc, tz, now)
	case ProviderECCC:
		return s.refreshECCC(ctx, loc, tz, now)
	}
	return fmt.Errorf("unknown weather provider %q", loc.Provider)
}

func fresh(exp *time.Time, now time.Time) bool { return exp != nil && now.Before(*exp) }

func (s *Live) refreshNWS(ctx context.Context, loc weatherLocation, tz *time.Location, now time.Time) error {
	if loc.GridOffice == nil || !fresh(loc.PointsExpiresAt, now) {
		pt, exp, ok, err := s.NWS.point(ctx, loc.Latitude, loc.Longitude, now)
		if err != nil {
			return err
		}
		if err := s.Store.savePoint(ctx, loc.Key, pt, optional(exp, ok)); err != nil {
			return err
		}
		loc.GridOffice, loc.GridX, loc.GridY = &pt.Office, &pt.X, &pt.Y
	}
	if !fresh(loc.ForecastExpiresAt, now) {
		days, ref, exp, ok, err := s.NWS.forecast(ctx, nwsPoint{Office: *loc.GridOffice, X: *loc.GridX, Y: *loc.GridY}, tz, now)
		if err != nil {
			return err
		}
		if err := s.Store.saveForecast(ctx, loc.Key, ProviderNWS, ref, days, today(now, tz), now, optional(exp, ok)); err != nil {
			return err
		}
	}
	if !fresh(loc.AlertsExpiresAt, now) {
		alerts, exp, ok, err := s.NWS.alerts(ctx, loc.Latitude, loc.Longitude, now)
		if err != nil {
			return err
		}
		return s.Store.saveAlerts(ctx, loc.Key, ProviderNWS, alerts, now, optional(exp, ok))
	}
	return nil
}

func (s *Live) refreshECCC(ctx context.Context, loc weatherLocation, tz *time.Location, now time.Time) error {
	if !fresh(loc.ForecastExpiresAt, now) {
		site := ""
		if loc.SiteCode != nil {
			site = *loc.SiteCode
		}
		var days []forecastDay
		var ref string
		var exp time.Time
		var ok bool
		var err error
		if site != "" {
			days, ref, exp, ok, err = s.ECCC.forecast(ctx, site, tz, now)
		}
		if site == "" || errors.Is(err, ErrNoProviderPoint) {
			if site, err = s.ECCC.site(ctx, loc.Latitude, loc.Longitude); err != nil {
				return err
			}
			if err = s.Store.saveSite(ctx, loc.Key, site); err != nil {
				return err
			}
			days, ref, exp, ok, err = s.ECCC.forecast(ctx, site, tz, now)
		}
		if err != nil {
			return err
		}
		if err := s.Store.saveForecast(ctx, loc.Key, ProviderECCC, ref, days, today(now, tz), now, optional(exp, ok)); err != nil {
			return err
		}
	}
	if !fresh(loc.AlertsExpiresAt, now) {
		alerts, exp, ok, err := s.ECCC.alerts(ctx, loc.Latitude, loc.Longitude, now)
		if err != nil {
			return err
		}
		return s.Store.saveAlerts(ctx, loc.Key, ProviderECCC, alerts, now, optional(exp, ok))
	}
	return nil
}

func today(now time.Time, tz *time.Location) string { return now.In(tz).Format(dateLayout) }

func optional(t time.Time, ok bool) *time.Time {
	if !ok {
		return nil
	}
	return &t
}

// getJSON maps transport failures, timeouts and non-200 replies to
// ErrProviderUnavailable, 404 to ErrNoProviderPoint and bad bodies to ErrInvalidSourceData.
func getJSON(ctx context.Context, client *http.Client, rawURL, userAgent, accept string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSourceData, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: HTTP 404", ErrNoProviderPoint)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", ErrProviderUnavailable, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, ctx.Err())
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidSourceData, err)
	}
	return resp.Header, nil
}

func celsiusToF(c float64) float64 { return math.Round((c*9/5+32)*100) / 100 }

type dayBuilder struct{ days map[string]*forecastDay }

func newDayBuilder() *dayBuilder { return &dayBuilder{days: map[string]*forecastDay{}} }

func (b *dayBuilder) add(date string, f float64, high bool) error {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < -100 || f > 150 {
		return fmt.Errorf("%w: temperature %v", ErrInvalidSourceData, f)
	}
	d := b.days[date]
	if d == nil {
		d = &forecastDay{Date: date}
		b.days[date] = d
	}
	v := f
	if high {
		d.HighF = &v
	} else {
		d.LowF = &v
	}
	return nil
}

func (b *dayBuilder) list() []forecastDay {
	out := make([]forecastDay, 0, len(b.days))
	for _, d := range b.days {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}
