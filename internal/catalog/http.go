// Package catalog serves reviewed product identities and their immutable facts.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var ErrNotFound = errors.New("catalog product not found")

type Search struct {
	Brand   string  `json:"brand"`
	Model   string  `json:"model"`
	Region  *string `json:"region,omitempty"`
	Variant *string `json:"variant,omitempty"`
	Fuel    string  `json:"fuel,omitempty"`
}

type Store interface {
	Search(context.Context, Search) ([]json.RawMessage, error)
	Get(context.Context, string) (json.RawMessage, error)
}

type Service struct {
	Store Store
	Auth  func(http.Handler) http.Handler
}

func (s Service) Routes(r chi.Router) {
	r.Route("/catalog", func(r chi.Router) {
		r.Use(s.Auth)
		r.Post("/model-search", s.search)
		r.Get("/products/{product}", s.get)
	})
}

func failure(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"code": code, "message": message, "request_id": uuid.NewString(), "details": map[string]any{}})
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s Service) search(w http.ResponseWriter, r *http.Request) {
	if _, err := uuid.Parse(r.Header.Get("Idempotency-Key")); err != nil {
		failure(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		failure(w, 415, "invalid_request", "application/json required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		failure(w, 400, "invalid_request", "invalid body")
		return
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var q Search
	if err = d.Decode(&q); err != nil {
		failure(w, 400, "invalid_request", "invalid JSON")
		return
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		failure(w, 400, "invalid_request", "one JSON object required")
		return
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(body, &fields); err != nil || fields == nil {
		failure(w, 400, "invalid_request", "one JSON object required")
		return
	}
	if raw, ok := fields["fuel"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		failure(w, 400, "invalid_request", "invalid model search")
		return
	}
	q.Brand, q.Model = strings.TrimSpace(q.Brand), strings.TrimSpace(q.Model)
	if q.Region != nil {
		region := strings.TrimSpace(*q.Region)
		q.Region = &region
	}
	if q.Variant != nil {
		variant := strings.TrimSpace(*q.Variant)
		q.Variant = &variant
	}
	if q.Fuel == "" {
		q.Fuel = "electricity"
	}
	if len(q.Brand) < 1 || len(q.Brand) > 120 || len(q.Model) < 2 || len(q.Model) > 120 || q.Region != nil && len(*q.Region) > 40 || q.Variant != nil && (len(*q.Variant) < 1 || len(*q.Variant) > 100) || q.Fuel != "electricity" && q.Fuel != "natural_gas" && q.Fuel != "propane" && q.Fuel != "fuel_oil" {
		failure(w, 400, "invalid_request", "invalid model search")
		return
	}
	products, err := s.Store.Search(r.Context(), q)
	if err != nil {
		slog.Error("catalog search failed", "error", err)
		failure(w, 500, "internal_error", "catalog search failed")
		return
	}
	respond(w, 200, map[string]any{"products": products})
}

func (s Service) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "product")
	if _, err := uuid.Parse(id); err != nil {
		failure(w, 404, "not_found", "catalog product not found")
		return
	}
	product, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		failure(w, 404, "not_found", "catalog product not found")
		return
	}
	if err != nil {
		slog.Error("catalog product lookup failed", "error", err)
		failure(w, 500, "internal_error", "catalog product lookup failed")
		return
	}
	respond(w, 200, product)
}
