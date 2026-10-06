// Package designfixture serves isolated, versioned mobile design baselines.
package designfixture

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"energylens.local/cloud/internal/account"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const Revision = "mobile_design_2026_10_06_v1"

//go:embed fixtures/*.json
var fixtures embed.FS

var pages = map[string]bool{
	"dashboard": true, "devices": true, "efficiency": true,
	"efficiency_away": true, "efficiency_peak_rates": true, "efficiency_habits": true,
	"perks_tips": true, "tips_efficiency": true, "tips_care_upgrades": true,
	"tips_knowledge": true, "settings": true, "settings_manage_devices": true,
	"settings_manual_device_entry": true, "settings_photo_scan_onboarding": true,
}

type HomeStore interface {
	Get(context.Context, string, string) (account.Home, error)
}

type Service struct {
	Homes        HomeStore
	Auth         func(http.Handler) http.Handler
	UserID       func(*http.Request) string
	AllowedUsers map[string]bool
	AllowedHomes map[string]bool
}

// ParseUserIDs accepts only explicit UUIDs. A malformed allowlist fails startup.
func ParseUserIDs(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, field := range strings.Split(raw, ",") {
		id := strings.TrimSpace(field)
		if id == "" {
			return nil, errors.New("empty sample fixture user ID")
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return nil, errors.New("invalid sample fixture user ID")
		}
		out[id] = true
	}
	return out, nil
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/design-pages/{page}", s.get)
}

func (s Service) get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	home, page := chi.URLParam(r, "home"), chi.URLParam(r, "page")
	if _, err := uuid.Parse(home); err != nil || !pages[page] {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	// No caller-provided mode, revision, or selector may change the baseline.
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	user := s.UserID(r)
	if !s.AllowedUsers[user] || !s.AllowedHomes[home] {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if _, err := s.Homes.Get(r.Context(), user, home); err != nil {
		if errors.Is(err, account.ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		slog.Error("sample fixture home ownership read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	b, err := fixtures.ReadFile("fixtures/" + page + ".json")
	if err != nil {
		slog.Error("sample fixture unavailable", "page", page, "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	var fixture struct {
		Revision string `json:"fixture_revision"`
		Page     string `json:"page"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil || fixture.Revision != Revision || fixture.Page != page {
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	out := struct {
		SchemaRevision  string          `json:"schema_revision"`
		HomeID          string          `json:"home_id"`
		Page            string          `json:"page"`
		DataMode        string          `json:"data_mode"`
		FixtureRevision string          `json:"fixture_revision"`
		SnapshotID      string          `json:"snapshot_id"`
		GeneratedAt     any             `json:"generated_at"`
		AsOf            any             `json:"as_of"`
		Timezone        any             `json:"timezone"`
		Period          any             `json:"period"`
		Scope           string          `json:"scope"`
		Availability    string          `json:"availability"`
		EvidenceBasis   string          `json:"evidence_basis"`
		ActionsEnabled  bool            `json:"actions_enabled"`
		Fixture         json.RawMessage `json:"fixture"`
	}{"mobile_design_page_v1", home, page, "sample", Revision, Revision + ":" + page,
		nil, nil, nil, nil, "page_specific_sample", "available", "page_fixture", false, b}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
