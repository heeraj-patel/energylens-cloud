package faq

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  EvidenceStore
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
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
		if finding := WeatherFinding(home, locationRevision, observations, now); finding != nil {
			in.ObservedPatterns = []Finding{*finding}
		}
	}
	if current != nil {
		in.AsOf = current.AsOf
		in.OperatingCost, _ = OperatingCostFromEstimate(current.Result, *requested)
	}
	answer, _ := Render(in)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
