package faq

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"energylens.local/cloud/internal/weather"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store   EvidenceStore
	Weather *weather.Service
	Auth    func(http.Handler) http.Handler
	UserID  func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/faq/answers/{question}", s.answer)
}

func (s Service) answer(w http.ResponseWriter, r *http.Request) {
	home, question := chi.URLParam(r, "home"), chi.URLParam(r, "question")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if !intents[question] {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	var appliance *string
	if raw := r.URL.Query().Get("appliance_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
		appliance = &raw
	}
	if question == "device_operating_cost" && appliance == nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	now := time.Now().UTC()
	var requested *string
	if question == "device_operating_cost" {
		requested = appliance
	}
	current, err := s.Store.CurrentEstimate(r.Context(), s.UserID(r), home, requested, now)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("FAQ evidence read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	in := Input{QuestionID: question, AsOf: now}
	var expires time.Time
	weatherStatus, weatherReason := "unavailable", "provider_unconfigured"
	if question == "high_bill_possible_contributors" {
		if s.Weather != nil {
			weatherStatus, weatherReason = "missing", "incomplete_coverage"
			loc, supported, locErr := s.Weather.Store.HomeLocation(r.Context(), s.UserID(r), home)
			if locErr != nil || !supported {
				weatherReason = "unsupported_location"
			}
			tz, tzErr := time.LoadLocation(loc.Timezone)
			if tzErr != nil {
				weatherReason = "invalid_timezone"
			}
			if supported && locErr == nil && tzErr == nil {
				var wg sync.WaitGroup
				limits := make(chan struct{}, 4)
				var mu sync.Mutex
				for ago := 14; ago >= 1; ago-- {
					wg.Add(1)
					go func(ago int) {
						defer wg.Done()
						limits <- struct{}{}
						defer func() { <-limits }()
						local := now.In(tz)
						day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -ago).Format("2006-01-02")
						result, err := s.Weather.Daily(r.Context(), s.UserID(r), home, day, now)
						if err != nil || result.Status != "available" {
							mu.Lock()
							if err != nil {
								weatherReason = "provider_or_cache_error"
							} else if weatherReason != "provider_or_cache_error" {
								weatherReason = result.Reason
							}
							mu.Unlock()
						}
					}(ago)
				}
				wg.Wait()
			}
		}
	}
	if question == "high_bill_possible_contributors" {
		observations, locationRevision, err := s.Store.CurrentWeather(r.Context(), s.UserID(r), home, now)
		if errors.Is(err, ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		if err != nil {
			slog.Error("FAQ weather evidence read failed", "error", err)
			fail(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if s.Weather != nil {
			filtered := observations[:0]
			for _, obs := range observations {
				if obs.Provider == s.Weather.Provider.Name() {
					filtered = append(filtered, obs)
				}
			}
			observations = filtered
		}
		if finding := WeatherFinding(home, locationRevision, observations, now); finding != nil {
			in.ObservedPatterns = []Finding{*finding}
			weatherStatus, weatherReason = "available", ""
			for _, obs := range observations {
				if expires.IsZero() || obs.ExpiresAt.Before(expires) {
					expires = obs.ExpiresAt
				}
			}
		} else if s.Weather == nil {
			weatherStatus, weatherReason = "missing", "provider_unconfigured"
		}
	}
	if question == "high_bill_possible_contributors" {
		if source, ok := s.Store.(interface {
			CurrentRecentUse(context.Context, string, string, *string) ([]RecentUse, error)
		}); ok {
			recent, err := source.CurrentRecentUse(r.Context(), s.UserID(r), home, appliance)
			if err != nil {
				slog.Error("FAQ recent-use read failed", "error", err)
				fail(w, http.StatusInternalServerError, "internal_error")
				return
			}
			findings, until := RecentUseFindings(recent, now)
			in.ObservedPatterns = append(in.ObservedPatterns, findings...)
			if !until.IsZero() && (expires.IsZero() || until.Before(expires)) {
				expires = until
			}
		}
	}
	if current != nil {
		in.OperatingCost, _ = OperatingCostFromEstimate(current.Result, *requested)
		if in.OperatingCost != nil {
			if periodEnd, err := time.Parse("2006-01-02", in.OperatingCost.PeriodEnd); err == nil { expires = periodEnd }
		}
	}
	answer, _ := Render(in)
	answer.InputSnapshot = append([]Evidence(nil), answer.EvidenceRefs...)
	if !expires.IsZero() && len(answer.Findings) > 0 {
		answer.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	if question == "high_bill_possible_contributors" {
		answer.WeatherStatus, answer.WeatherReason = weatherStatus, weatherReason
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
