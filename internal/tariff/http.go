package tariff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	r.Route("/homes/{home}/tariffs", func(r chi.Router) {
		r.Use(s.Auth)
		r.Get("/", s.list)
		r.Post("/", s.create)
	})
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"code": code, "message": message, "request_id": uuid.NewString(), "details": map[string]any{}})
}
func homeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "home")
	if _, err := uuid.Parse(id); err != nil {
		fail(w, 404, "not_found", "home not found")
		return "", false
	}
	return id, true
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found", "home not found")
	case errors.Is(err, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse", "key used with another request")
	default:
		slog.Error("tariff store failed", "error", err)
		fail(w, 500, "internal_error", "tariff request failed")
	}
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	tariffs, err := s.Store.List(r.Context(), s.UserID(r), home)
	if err != nil {
		storeError(w, err)
		return
	}
	response := map[string]any{"tariffs": tariffs}
	if reader, ok := s.Store.(interface {
		Research(context.Context, string, string) ([]datawizfacts.Fact, error)
	}); ok {
		research, err := reader.Research(r.Context(), s.UserID(r), home)
		if err != nil {
			storeError(w, err)
			return
		}
		response["research_facts"] = research
	}
	respond(w, 200, response)
}
func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		fail(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		fail(w, 415, "invalid_request", "application/json required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		fail(w, 400, "invalid_request", "invalid body")
		return
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var in Create
	if err = d.Decode(&in); err != nil {
		fail(w, 400, "invalid_request", "invalid JSON")
		return
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, 400, "invalid_request", "one JSON object required")
		return
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(body, &fields); err != nil || fields == nil {
		fail(w, 400, "invalid_request", "one JSON object required")
		return
	}
	for _, field := range []string{"fuel", "rate", "rate_unit", "currency", "rate_kind", "source_ref", "source_as_of", "is_fallback", "fixed_charge", "effective_from", "effective_to"} {
		if _, ok := fields[field]; !ok {
			fail(w, 400, "invalid_request", "missing tariff field")
			return
		}
	}
	if !in.Valid(time.Now().UTC()) {
		fail(w, 400, "invalid_request", "invalid tariff revision")
		return
	}
	hash := sha256.Sum256(body)
	tariff, err := s.Store.Create(r.Context(), s.UserID(r), home, in, key, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 201, tariff)
}
