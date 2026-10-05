// Package usagefactapi serves active, operator-reviewed usage facts for exact
// device identities. It never computes home estimates or savings.
package usagefactapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Scope struct {
	Category     string `json:"category"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Fuel         string `json:"fuel"`
	Variant      string `json:"variant"`
}

type Fact struct {
	ID                string          `json:"id"`
	MeasurementKind   string          `json:"measurement_kind"`
	Value             string          `json:"value"`
	Unit              string          `json:"unit"`
	TestBasis         string          `json:"test_basis"`
	CycleDefinition   *string         `json:"cycle_definition"`
	OperationMode     *string         `json:"operation_mode"`
	IncludedEnergy    json.RawMessage `json:"included_energy"`
	SourceKind        string          `json:"source_kind"`
	SourceURL         string          `json:"source_url"`
	SourceSHA256      string          `json:"source_sha256"`
	SourcePageIndex   *int            `json:"source_page_index"`
	DatasetID         *string         `json:"dataset_id"`
	RowIdentifier     *string         `json:"row_identifier"`
	FieldName         *string         `json:"field_name"`
	MetadataSHA256    *string         `json:"metadata_sha256"`
	ReviewSHA256      string          `json:"review_sha256"`
	ReviewedAt        string          `json:"reviewed_at"`
	PublicationSHA256 string          `json:"publication_sha256"`
}

type Store interface {
	Find(context.Context, Scope) ([]Fact, error)
}
type CertificateStore interface {
	VerifyCertificate(context.Context, string, string, int64) (hubtrust.Hub, error)
}
type Service struct {
	Store           Store
	Auth            func(http.Handler) http.Handler
	HubCertificates CertificateStore
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/usage-facts", s.get)
}
func (s Service) HubRoutes(r chi.Router) {
	r.With(HubAuthentication(s.HubCertificates)).Get("/hubs/{hub}/usage-facts", s.get)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": code, "request_id": uuid.NewString(), "details": map[string]any{}})
}
func validField(s string, max int) bool {
	if len(s) < 1 || len(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func scopeFromRequest(r *http.Request) (Scope, bool) {
	q := r.URL.Query()
	for key, values := range q {
		if key != "category" && key != "manufacturer" && key != "model" && key != "fuel" && key != "variant" || len(values) != 1 {
			return Scope{}, false
		}
	}
	s := Scope{q.Get("category"), q.Get("manufacturer"), q.Get("model"), q.Get("fuel"), q.Get("variant")}
	if !validField(s.Category, 60) || !validField(s.Manufacturer, 120) || !validField(s.Model, 120) || !validField(s.Variant, 100) {
		return Scope{}, false
	}
	if s.Fuel != "electricity" && !(s.Fuel == "natural_gas" && s.Category == "dryer") {
		return Scope{}, false
	}
	return s, true
}
func (s Service) get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	scope, ok := scopeFromRequest(r)
	if !ok {
		fail(w, 400, "invalid_request")
		return
	}
	facts, err := s.Store.Find(r.Context(), scope)
	if err != nil {
		slog.Error("reviewed usage lookup failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	if len(facts) > 2 {
		slog.Error("ambiguous reviewed usage scope", "scope", scope)
		fail(w, 500, "internal_error")
		return
	}
	if facts == nil {
		facts = []Fact{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Scope         Scope  `json:"scope"`
		Facts         []Fact `json:"facts"`
	}{"energylens.reviewed-usage-facts", 1, scope, facts})
}

// HubAuthentication verifies the TLS peer against current durable hub state.
func HubAuthentication(store CertificateStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "private, no-store")
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
				fail(w, 403, "forbidden")
				return
			}
			peer := r.TLS.PeerCertificates[0]
			hubID, epoch, ok := hubtrust.PeerIdentity(peer)
			if !ok || hubID != chi.URLParam(r, "hub") || peer.SerialNumber == nil {
				fail(w, 403, "forbidden")
				return
			}
			hub, err := store.VerifyCertificate(r.Context(), hubID, peer.SerialNumber.Text(16), epoch)
			if errors.Is(err, hubtrust.ErrDenied) {
				fail(w, 403, "forbidden")
				return
			}
			if err != nil {
				fail(w, 503, "unavailable")
				return
			}
			if hub.ID != hubID || hub.Epoch != epoch || hub.Revoked || hub.HomeID == nil {
				fail(w, 403, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
