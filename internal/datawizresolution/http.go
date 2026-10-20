package datawizresolution

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/usagefactapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store           Store
	Auth            func(http.Handler) http.Handler
	UserID          func(*http.Request) string
	HubCertificates hubtrust.Store
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/appliances/{appliance}/energy-resolution", s.owner)
	r.With(s.Auth).Post("/homes/{home}/appliances/{appliance}/energy-resolution/refresh", s.refresh)
}
func (s Service) HubRoutes(r chi.Router) {
	r.With(usagefactapi.HubAuthentication(s.HubCertificates)).Get("/hubs/{hub}/energy-resolutions/{appliance}", s.hub)
}
func failure(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": http.StatusText(status), "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func failureWith(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "request_id": uuid.NewString(), "details": details})
}
func (s Service) owner(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	if _, err := uuid.Parse(home); err != nil {
		failure(w, 404)
		return
	}
	if _, err := uuid.Parse(appliance); err != nil {
		failure(w, 404)
		return
	}
	s.serve(w, r, func() (json.RawMessage, error) { return s.Store.ForOwner(r.Context(), s.UserID(r), home, appliance) })
}
func (s Service) refresh(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	if _, err := uuid.Parse(home); err != nil {
		failure(w, 404)
		return
	}
	if _, err := uuid.Parse(appliance); err != nil {
		failure(w, 404)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		failureWith(w, 400, "invalid_request", "UUID Idempotency-Key required", nil)
		return
	}
	if body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1)); err != nil || len(body) != 0 {
		failureWith(w, 400, "invalid_request", "request body not allowed", nil)
		return
	}
	result, err := s.Store.Refresh(r.Context(), s.UserID(r), home, appliance, key)
	var limited RateLimited
	switch {
	case errors.Is(err, ErrNotFound):
		failure(w, 404)
		return
	case errors.As(err, &limited):
		seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		failureWith(w, 429, "refresh_rate_limited", "refresh requested too recently", map[string]any{"retry_after_seconds": seconds})
		return
	case err != nil:
		slog.Error("energy resolution refresh failed", "error", err)
		failure(w, 500)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(result)
}
func (s Service) hub(w http.ResponseWriter, r *http.Request) {
	hub, appliance := chi.URLParam(r, "hub"), chi.URLParam(r, "appliance")
	if _, err := uuid.Parse(hub); err != nil {
		failure(w, 404)
		return
	}
	if _, err := uuid.Parse(appliance); err != nil {
		failure(w, 404)
		return
	}
	s.serve(w, r, func() (json.RawMessage, error) { return s.Store.ForHub(r.Context(), hub, appliance) })
}
func (s Service) serve(w http.ResponseWriter, r *http.Request, read func() (json.RawMessage, error)) {
	w.Header().Set("Cache-Control", "private, no-store")
	data, err := read()
	if errors.Is(err, ErrNotFound) {
		failure(w, 404)
		return
	}
	if err != nil {
		failure(w, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(data, '\n'))
}
