package estimation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var ErrNotFound = errors.New("home or appliance not found")

type RunStore interface {
	ListRuns(context.Context, string, string, *string, time.Time) ([]json.RawMessage, error)
}
type Service struct {
	Store  RunStore
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.Route("/homes/{home}/estimates", func(r chi.Router) { r.Use(s.Auth); r.Get("/", s.list) })
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		estimateError(w, 404, "not_found", "home not found")
		return
	}
	var appliance *string
	if raw := r.URL.Query().Get("appliance_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			estimateError(w, 400, "invalid_request", "invalid appliance_id")
			return
		}
		appliance = &raw
	}
	now := time.Now().UTC()
	runs, err := s.Store.ListRuns(r.Context(), s.UserID(r), home, appliance, now)
	if errors.Is(err, ErrNotFound) {
		estimateError(w, 404, "not_found", "home or appliance not found")
		return
	}
	if err != nil {
		slog.Error("estimate runs failed", "error", err)
		estimateError(w, 500, "internal_error", "estimate request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"estimates": runs, "as_of": now.Format(time.RFC3339Nano)})
}
func estimateError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "request_id": uuid.NewString(), "details": map[string]any{}})
}
