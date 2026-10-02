// Package weather resolves daily observations by coarse home location. It
// never turns weather alone into an explanation of a customer's bill.
package weather

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var ErrHomeNotFound = errors.New("home not found")

type Location struct {
	CellKey          string
	Timezone         string
	HomeVersion      int64
	LocationRevision int64
}

// CoarseCell currently supports US ZIP3 and Canadian FSA only. In particular,
// an address and complete postal code are never sent to a weather provider.
func CoarseCell(country, postal, timezone string) (string, bool) {
	postal = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(postal), " ", ""))
	switch strings.ToUpper(country) {
	case "US":
		if !regexp.MustCompile(`^[0-9]{5}(-[0-9]{4})?$`).MatchString(postal) {
			return "", false
		}
		return "US:" + postal[:3] + ":" + timezone, true
	case "CA":
		if !regexp.MustCompile(`^[A-Z][0-9][A-Z][0-9][A-Z][0-9]$`).MatchString(postal) {
			return "", false
		}
		return "CA:" + postal[:3] + ":" + timezone, true
	default:
		return "", false
	}
}

type Observation struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	CellKey          string    `json:"cell_key"`
	LocalDate        string    `json:"local_date"`
	Timezone         string    `json:"timezone"`
	MeanTemperatureC float64   `json:"mean_temperature_c"`
	SourceRevision   string    `json:"source_revision"`
	RetrievedAt      time.Time `json:"retrieved_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type Dependency struct {
	HomeVersion      int64     `json:"home_version"`
	LocationRevision int64     `json:"location_revision"`
	ObservationID    string    `json:"observation_id"`
	Provider         string    `json:"provider"`
	SourceRevision   string    `json:"source_revision"`
	RetrievedAt      time.Time `json:"retrieved_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type Result struct {
	Status      string       `json:"status"` // available or missing
	Reason      string       `json:"reason,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
	Dependency  *Dependency  `json:"dependency,omitempty"`
}

// Provider receives only a coarse cell, home timezone and day. Implementations
// must return an actual observed daily mean with a traceable source revision.
type Provider interface {
	Name() string
	FetchDaily(context.Context, string, string, string) (float64, string, error)
}

type Store interface {
	HomeLocation(context.Context, string, string) (Location, bool, error)
	Latest(context.Context, string, string, string, string) (*Observation, error)
	Save(context.Context, Observation) (Observation, error)
}

type Service struct {
	Store    Store
	Provider Provider
	TTL      time.Duration
}

// Daily returns missing when geography is unsupported, the source fails, or
// the latest cache row has expired. Expired observations are never promoted to
// current evidence if a refresh fails.
func (s Service) Daily(ctx context.Context, owner, home, day string, now time.Time) (Result, error) {
	requestedDay, err := time.Parse("2006-01-02", day)
	if err != nil {
		return Result{}, fmt.Errorf("invalid local date: %w", err)
	}
	loc, supported, err := s.Store.HomeLocation(ctx, owner, home)
	if err != nil {
		return Result{}, err
	}
	if !supported {
		return Result{Status: "missing", Reason: "unsupported_location"}, nil
	}
	tz, err := time.LoadLocation(loc.Timezone)
	if err != nil {
		return Result{}, fmt.Errorf("invalid home timezone: %w", err)
	}
	localNow := now.In(tz)
	if !requestedDay.Before(time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC)) {
		return Result{Status: "missing", Reason: "incomplete_day"}, nil
	}
	if s.Provider == nil || s.Provider.Name() == "" || s.TTL <= 0 || s.TTL > 7*24*time.Hour {
		return Result{}, errors.New("weather provider and positive TTL required")
	}
	previous, err := s.Store.Latest(ctx, s.Provider.Name(), loc.CellKey, day, loc.Timezone)
	if err != nil {
		return Result{}, err
	}
	if previous != nil && !now.Before(previous.RetrievedAt) && now.Before(previous.ExpiresAt) {
		return available(*previous, loc), nil
	}
	mean, revision, err := s.Provider.FetchDaily(ctx, loc.CellKey, loc.Timezone, day)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if previous != nil {
			return Result{Status: "missing", Reason: "stale_weather"}, nil
		}
		return Result{Status: "missing", Reason: "provider_unavailable"}, nil
	}
	if math.IsNaN(mean) || math.IsInf(mean, 0) || mean < -100 || mean > 70 || revision == "" || len(revision) > 160 {
		return Result{Status: "missing", Reason: "invalid_source_data"}, nil
	}
	obs, err := s.Store.Save(ctx, Observation{Provider: s.Provider.Name(), CellKey: loc.CellKey, LocalDate: day, Timezone: loc.Timezone, MeanTemperatureC: mean, SourceRevision: revision, RetrievedAt: now.UTC(), ExpiresAt: now.Add(s.TTL).UTC()})
	if err != nil {
		return Result{}, err
	}
	return available(obs, loc), nil
}

func available(obs Observation, loc Location) Result {
	return Result{Status: "available", Observation: &obs, Dependency: &Dependency{HomeVersion: loc.HomeVersion, LocationRevision: loc.LocationRevision, ObservationID: obs.ID, Provider: obs.Provider, SourceRevision: obs.SourceRevision, RetrievedAt: obs.RetrievedAt, ExpiresAt: obs.ExpiresAt}}
}
