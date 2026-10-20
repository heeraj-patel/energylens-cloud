package cyclecomparison

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/appliances/{appliance}/cycle-comparison/reference", s.reference)
	r.With(s.Auth).Post("/homes/{home}/appliances/{appliance}/cycle-comparison", s.compare)
	r.With(s.Auth).Post("/homes/{home}/appliances/{appliance}/cycle-comparison/evidence", s.save)
	r.With(s.Auth).Get("/homes/{home}/appliances/{appliance}/cycle-comparison/evidence/{id}", s.saved)
}

func validID(x string) bool { u, e := uuid.Parse(x); return e == nil && u.String() == x }
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}

func valid(in Request, now time.Time) bool {
	return validID(in.AttachmentID) && validID(in.FactID) && !in.Start.IsZero() && in.End.After(in.Start) && in.End.Sub(in.Start) <= 24*time.Hour && !in.Start.Before(now.Add(-30*24*time.Hour)) && !in.End.After(now) && strings.TrimSpace(in.ConfirmedTestBasis) == in.ConfirmedTestBasis && in.ConfirmedTestBasis != "" && strings.TrimSpace(in.ConfirmedCycleDefinition) == in.ConfirmedCycleDefinition && in.ConfirmedCycleDefinition != ""
}

func (s Service) reference(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	if !validID(home) || !validID(appliance) {
		fail(w, 404, "not_found")
		return
	}
	out, err := s.Store.Reference(r.Context(), s.UserID(r), home, appliance)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("cycle reference lookup failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s Service) compare(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	if !validID(home) || !validID(appliance) {
		fail(w, 404, "not_found")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "unsupported_media_type")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	var in Request
	if err := d.Decode(&in); err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, "invalid_request")
		return
	}
	now := time.Now().UTC()
	if !valid(in, now) {
		fail(w, 400, "invalid_request")
		return
	}
	out, err := s.Store.Compare(r.Context(), s.UserID(r), home, appliance, in, now)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("cycle comparison failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s Service) save(w http.ResponseWriter, r *http.Request) {
	home, appliance := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	key := r.Header.Get("Idempotency-Key")
	if !validID(home) || !validID(appliance) {
		fail(w, 404, "not_found")
		return
	}
	if !validID(key) {
		fail(w, 400, "invalid_idempotency_key")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "unsupported_media_type")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	var in Request
	if d.Decode(&in) != nil {
		fail(w, 400, "invalid_request")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "invalid_request")
		return
	}
	now := time.Now().UTC()
	if !valid(in, now) {
		fail(w, 400, "invalid_request")
		return
	}
	out, err := s.Store.Save(r.Context(), s.UserID(r), home, appliance, key, in, now)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found")
		return
	case errors.Is(err, ErrIdempotencyConflict):
		fail(w, 409, "idempotency_conflict")
		return
	case errors.Is(err, ErrEvidenceUnavailable):
		fail(w, 409, "evidence_unavailable")
		return
	case err != nil:
		slog.Error("cycle evidence save failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(out)
}

func (s Service) saved(w http.ResponseWriter, r *http.Request) {
	home, appliance, id := chi.URLParam(r, "home"), chi.URLParam(r, "appliance"), chi.URLParam(r, "id")
	if !validID(home) || !validID(appliance) || !validID(id) {
		fail(w, 404, "not_found")
		return
	}
	out, err := s.Store.Get(r.Context(), s.UserID(r), home, appliance, id, time.Now().UTC())
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("cycle evidence read failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
