package datawizresolution

import (
	"encoding/json"
	"errors"
	"net/http"

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
}
func (s Service) HubRoutes(r chi.Router) {
	r.With(usagefactapi.HubAuthentication(s.HubCertificates)).Get("/hubs/{hub}/energy-resolutions/{appliance}", s.hub)
}
func failure(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": http.StatusText(status), "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
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
