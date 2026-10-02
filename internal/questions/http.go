package questions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.Route("/homes/{home}/questions", func(r chi.Router) {
		r.Use(s.Auth)
		r.Get("/", s.list)
		r.Get("/answers", s.currentAnswers)
		r.Post("/answers", s.submit)
	})
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code, msg string) {
	write(w, status, map[string]any{"code": code, "message": msg, "request_id": uuid.NewString(), "details": map[string]any{}})
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found", "home or appliance not found")
	case errors.Is(err, ErrVersion):
		fail(w, 409, "version_conflict", "appliance identity revision changed")
	case errors.Is(err, ErrInvalid):
		fail(w, 400, "invalid_request", "invalid question answer")
	case errors.Is(err, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse", "key used with another request")
	case errors.Is(err, ErrDisclosureRequired):
		fail(w, 403, "history_disclosure_required", "accept history disclosure before answering questions")
	default:
		slog.Error("question store failed", "error", err)
		fail(w, 500, "internal_error", "question request failed")
	}
}
func homeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, 404, "not_found", "home not found")
		return "", false
	}
	return home, true
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	purpose := r.URL.Query().Get("purpose")
	if purpose != "onboarding" && purpose != "avoid" && purpose != "faq" {
		fail(w, 400, "invalid_request", "purpose required")
		return
	}
	var appliance *string
	if raw := r.URL.Query().Get("appliance_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			fail(w, 400, "invalid_request", "invalid appliance_id")
			return
		}
		appliance = &raw
	}
	questions, err := s.Store.List(r.Context(), s.UserID(r), home, purpose, appliance)
	if err != nil {
		storeError(w, err)
		return
	}
	write(w, 200, map[string]any{"questions": questions})
}
func (s Service) submit(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		fail(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request", "application/json required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		fail(w, 400, "invalid_request", "invalid body")
		return
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var input struct {
		Answers []AnswerCreate `json:"answers"`
	}
	if err = d.Decode(&input); err != nil {
		fail(w, 400, "invalid_request", "invalid JSON")
		return
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, 400, "invalid_request", "one JSON object required")
		return
	}
	if len(input.Answers) < 1 || len(input.Answers) > 50 {
		fail(w, 400, "invalid_request", "1 to 50 answers required")
		return
	}
	hash := sha256.Sum256(body)
	answers, err := s.Store.Submit(r.Context(), s.UserID(r), home, input.Answers, key, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	write(w, 201, map[string]any{"answers": answers})
}

func (s Service) currentAnswers(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	appliance := r.URL.Query().Get("appliance_id")
	if _, err := uuid.Parse(appliance); err != nil {
		fail(w, 400, "invalid_request", "appliance_id required")
		return
	}
	answers, err := s.Store.CurrentAnswers(r.Context(), s.UserID(r), home, appliance)
	if err != nil {
		storeError(w, err)
		return
	}
	write(w, 200, map[string]any{"answers": answers})
}
