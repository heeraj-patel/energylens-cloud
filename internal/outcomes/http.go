package outcomes

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
	r.With(s.Auth).Post("/homes/{home}/interventions", s.create)
	r.With(s.Auth).Get("/homes/{home}/interventions/{id}/outcome", s.outcome)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func validID(x string) bool { u, e := uuid.Parse(x); return e == nil && u.String() == x }

func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home, key := chi.URLParam(r, "home"), r.Header.Get("Idempotency-Key")
	if !validID(home) {
		fail(w, 404, "not_found")
		return
	}
	if !validID(key) {
		fail(w, 400, "invalid_request")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		fail(w, 415, "unsupported_media_type")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var in Create
	if err := d.Decode(&in); err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, "invalid_request")
		return
	}
	if !ValidCreate(in, time.Now().UTC()) {
		fail(w, 400, "invalid_request")
		return
	}
	a, err := s.Store.CreateAction(r.Context(), s.UserID(r), home, key, in)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if errors.Is(err, ErrConflict) {
		fail(w, 409, "conflict")
		return
	}
	if err != nil {
		slog.Error("intervention create failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": a.ID, "status": "completed", "outcome_status": "collecting"})
}

func (s Service) outcome(w http.ResponseWriter, r *http.Request) {
	home, id := chi.URLParam(r, "home"), chi.URLParam(r, "id")
	if !validID(home) || !validID(id) {
		fail(w, 404, "not_found")
		return
	}
	a, err := s.Store.GetAction(r.Context(), s.UserID(r), home, id)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("intervention read failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	ready, err := s.Store.Ready(r.Context(), s.UserID(r), home, a)
	if err != nil {
		slog.Error("outcome rollup readiness failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	if !ready {
		o := Evaluate(a, nil, time.Now().UTC())
		o.Status = "collecting"
		o.Reasons = []string{"rollup_pending"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(o)
		return
	}
	rows, err := s.Store.Buckets(r.Context(), s.UserID(r), home, a)
	if err != nil {
		slog.Error("outcome evidence read failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Evaluate(a, rows, time.Now().UTC()))
}
