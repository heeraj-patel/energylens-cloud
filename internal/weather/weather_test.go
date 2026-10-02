package weather

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProvider struct {
	calls    int
	fail     bool
	mean     float64
	revision string
}

func (p *fakeProvider) Name() string { return "fake-weather" }
func (p *fakeProvider) FetchDaily(_ context.Context, cell, timezone, day string) (float64, string, error) {
	p.calls++
	if cell != "US:787:America/Chicago" || timezone != "America/Chicago" || day != "2026-09-30" {
		return 0, "", errors.New("precise or unexpected location")
	}
	if p.fail {
		return 0, "", errors.New("source offline")
	}
	return p.mean, p.revision, nil
}

type memoryStore struct {
	loc   Location
	obs   []Observation
	owner string
}

func (s *memoryStore) HomeLocation(_ context.Context, owner, _ string) (Location, bool, error) {
	if owner != s.owner {
		return Location{}, false, ErrHomeNotFound
	}
	return s.loc, s.loc.CellKey != "", nil
}
func (s *memoryStore) Latest(_ context.Context, provider, cell, day, timezone string) (*Observation, error) {
	for i := len(s.obs) - 1; i >= 0; i-- {
		o := s.obs[i]
		if o.Provider == provider && o.CellKey == cell && o.LocalDate == day && o.Timezone == timezone {
			return &o, nil
		}
	}
	return nil, nil
}
func (s *memoryStore) Save(_ context.Context, o Observation) (Observation, error) {
	o.ID = "weather-" + string(rune('1'+len(s.obs)))
	s.obs = append(s.obs, o)
	return o, nil
}

func TestCoarseCell(t *testing.T) {
	for _, tc := range []struct{ country, postal, want string }{
		{"US", "78732", "US:787:America/Chicago"},
		{"CA", "M5V 2T6", "CA:M5V:America/Chicago"},
		{"US", "787", ""},
		{"GB", "SW1A 1AA", ""},
	} {
		got, _ := CoarseCell(tc.country, tc.postal, "America/Chicago")
		if got != tc.want {
			t.Fatalf("cell %q/%q: got %q want %q", tc.country, tc.postal, got, tc.want)
		}
	}
}

func TestDailyCacheFreshnessAndDependencies(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{owner: "owner", loc: Location{CellKey: "US:787:America/Chicago", Timezone: "America/Chicago", HomeVersion: 1, LocationRevision: 1}}
	provider := &fakeProvider{mean: 23.5, revision: "daily-source-1"}
	svc := Service{Store: store, Provider: provider, TTL: 24 * time.Hour}
	first, err := svc.Daily(context.Background(), "owner", "home", "2026-09-30", now)
	if err != nil || first.Status != "available" || first.Dependency.LocationRevision != 1 || first.Dependency.ObservationID == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := svc.Daily(context.Background(), "owner", "home", "2026-09-30", now.Add(time.Hour))
	if err != nil || provider.calls != 1 || second.Dependency.ObservationID != first.Dependency.ObservationID {
		t.Fatalf("cache: %+v %v calls=%d", second, err, provider.calls)
	}
	provider.fail = true
	stale, err := svc.Daily(context.Background(), "owner", "home", "2026-09-30", now.Add(25*time.Hour))
	if err != nil || stale.Status != "missing" || stale.Reason != "stale_weather" || stale.Observation != nil {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	provider.fail = false
	provider.revision = "daily-source-2"
	store.loc.HomeVersion, store.loc.LocationRevision = 2, 2
	refreshed, err := svc.Daily(context.Background(), "owner", "home", "2026-09-30", now.Add(26*time.Hour))
	if err != nil || refreshed.Status != "available" || refreshed.Dependency.ObservationID == first.Dependency.ObservationID || refreshed.Dependency.LocationRevision != 2 || refreshed.Dependency.SourceRevision != "daily-source-2" {
		t.Fatalf("refresh: %+v %v", refreshed, err)
	}
	if _, err := svc.Daily(context.Background(), "other", "home", "2026-09-30", now); !errors.Is(err, ErrHomeNotFound) {
		t.Fatalf("other owner: %v", err)
	}
}

func TestMissingWeatherNeverBecomesEvidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{owner: "owner", loc: Location{Timezone: "America/Chicago"}}
	p := &fakeProvider{mean: 200, revision: "bad"}
	svc := Service{Store: store, Provider: p, TTL: time.Hour}
	r, err := svc.Daily(context.Background(), "owner", "home", "2026-09-30", now)
	if err != nil || r.Status != "missing" || r.Reason != "unsupported_location" || p.calls != 0 {
		t.Fatalf("unsupported: %+v %v", r, err)
	}
	store.loc.CellKey = "US:787:America/Chicago"
	r, err = svc.Daily(context.Background(), "owner", "home", "2026-10-01", now)
	if err != nil || r.Status != "missing" || r.Reason != "incomplete_day" || p.calls != 0 {
		t.Fatalf("incomplete day: %+v %v", r, err)
	}
	r, err = svc.Daily(context.Background(), "owner", "home", "2026-09-30", now)
	if err != nil || r.Status != "missing" || r.Reason != "invalid_source_data" || r.Dependency != nil {
		t.Fatalf("invalid source: %+v %v", r, err)
	}
}
