// Package ratedinputapi serves active signed reviewed nameplate facts to Hub.
package ratedinputapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"log/slog"
	"net/http"
	"strings"

	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/usagefactapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Scope struct {
	Category     string `json:"category"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
}
type Fact struct {
	ID                    string `json:"id"`
	Category              string `json:"category"`
	Manufacturer          string `json:"manufacturer"`
	Model                 string `json:"model"`
	SourceKind            string `json:"source_kind"`
	MatchedModel          string `json:"matched_model"`
	RatedInputWatts       string `json:"rated_input_watts"`
	Unit                  string `json:"unit"`
	SourceURL             string `json:"source_url"`
	ManufacturerSourceURL string `json:"manufacturer_source_url"`
	SourceSHA256          string `json:"source_sha256"`
	ReviewSHA256          string `json:"review_sha256"`
	Reviewer              string `json:"reviewer"`
	ReviewedAt            string `json:"reviewed_at"`
	UsageBasis            string `json:"usage_basis"`
	SourceManifest        string `json:"source_manifest"`
	PublicationSHA256     string `json:"publication_sha256"`
}
type Store interface {
	Find(context.Context, Scope) ([]Fact, error)
}
type CertificateStore interface {
	VerifyCertificate(context.Context, string, string, int64) (hubtrust.Hub, error)
}
type Service struct {
	Store           Store
	HubCertificates CertificateStore
}

func (s Service) HubRoutes(r chi.Router) {
	r.With(usagefactapi.HubAuthentication(s.HubCertificates)).Get("/hubs/{hub}/rated-input", s.get)
}
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": code, "request_id": uuid.NewString(), "details": map[string]any{}})
}
func valid(s string, max int) bool {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func (s Service) get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	for key, values := range q {
		if (key != "category" && key != "manufacturer" && key != "model") || len(values) != 1 {
			fail(w, 400, "invalid_request")
			return
		}
	}
	scope := Scope{q.Get("category"), q.Get("manufacturer"), q.Get("model")}
	if !valid(scope.Category, 60) || !valid(scope.Manufacturer, 120) || !valid(scope.Model, 120) {
		fail(w, 400, "invalid_request")
		return
	}
	facts, err := s.Store.Find(r.Context(), scope)
	if err != nil {
		slog.Error("reviewed rated-input lookup failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	if len(facts) > 1 {
		slog.Error("ambiguous reviewed rated-input identity", "scope", scope)
		fail(w, 500, "internal_error")
		return
	}
	if facts == nil {
		facts = []Fact{}
	}
	var research []datawizfacts.Fact
	if reader, ok := s.Store.(interface {
		Research(context.Context, Scope) ([]datawizfacts.Fact, error)
	}); ok {
		research, err = reader.Research(r.Context(), scope)
		if err != nil {
			fail(w, 500, "internal_error")
			return
		}
	}
	body, err := json.Marshal(struct {
		Schema        string              `json:"schema"`
		SchemaVersion int                 `json:"schema_version"`
		Scope         Scope               `json:"scope"`
		Facts         []Fact              `json:"facts"`
		ResearchFacts []datawizfacts.Fact `json:"research_facts,omitempty"`
	}{"energylens.reviewed-rated-input-specs", 1, scope, facts, research})
	if err != nil {
		fail(w, 500, "internal_error")
		return
	}
	digest := sha256.Sum256(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-EnergyLens-Content-SHA256", hex.EncodeToString(digest[:]))
	_, _ = w.Write(body)
}
