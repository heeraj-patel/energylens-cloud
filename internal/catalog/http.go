// Package catalog serves reviewed product identities and their immutable facts.
package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	SnapshotSearch(context.Context, Search) ([]json.RawMessage, int64, error)
	SnapshotGet(context.Context, string) (json.RawMessage, int64, error)
}

type Service struct {
	Store   Store
	Auth    func(http.Handler) http.Handler
	HubAuth func(http.Handler) http.Handler
}

// HubRoutes shares the reviewed store and response format with the owner API.
// Authentication is checked before parsing the request or reading catalog data.
func (s Service) HubRoutes(r chi.Router) {
	r.Route("/hubs/{hub}/catalog", func(r chi.Router) {
		r.Use(s.HubAuth)
		r.Post("/model-search", s.search)
		r.Get("/products/{product}", s.get)
	})
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

func respondCatalog(w http.ResponseWriter, value any, generation int64) {
	body, err := json.Marshal(value)
	if err != nil {
		failure(w, 500, "internal_error", "catalog response failed")
		return
	}
	body = append(body, '\n')
	digest := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(digest[:])
	w.Header().Set("X-Catalog-Generation", strconv.FormatInt(generation, 10))
	w.Header().Set("X-Catalog-Response-SHA256", hexDigest)
	w.Header().Set("ETag", `"sha256-`+hexDigest+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
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
	products, generation, err := s.Store.SnapshotSearch(r.Context(), q)
	if err != nil {
		slog.Error("catalog search failed", "error", err)
		failure(w, 500, "internal_error", "catalog search failed")
		return
	}
	respondCatalog(w, map[string]any{"products": products}, generation)
}

func (s Service) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "product")
	if _, err := uuid.Parse(id); err != nil {
		failure(w, 404, "not_found", "catalog product not found")
		return
	}
	product, generation, err := s.Store.SnapshotGet(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		failure(w, 404, "not_found", "catalog product not found")
		return
	}
	if err != nil {
		slog.Error("catalog product lookup failed", "error", err)
		failure(w, 500, "internal_error", "catalog product lookup failed")
		return
	}
	respondCatalog(w, product, generation)
}
