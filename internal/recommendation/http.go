package recommendation

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  EvidenceStore
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
	Waste  interface {
		DeviceMonthlyWaste(ctx context.Context, owner, home string) ([]householdenergy.DeviceWaste, error)
	}
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/recommendations", s.list)
	r.With(s.Auth).Get("/homes/{home}/top-savings-move", s.topSavingsMove)
	r.With(s.Auth).Post("/homes/{home}/recommendation-events", s.record)
}

func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	now := time.Now().UTC()
	appliances, err := s.Store.Current(r.Context(), s.UserID(r), home, now)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("recommendation evidence read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	actions := []Action{}
	evaluate := evaluatorFor(r.URL.Query().Get("capability"))
	for _, appliance := range appliances {
		actions = append(actions, evaluate(appliance)...)
	}
	actions = rankActions(actions)
	var research []datawizfacts.Fact
	if reader, ok := s.Store.(interface {
		Research(context.Context) ([]datawizfacts.Fact, error)
	}); ok {
		research, err = reader.Research(r.Context())
		if err != nil {
			slog.Error("recommendation research read failed", "error", err)
			fail(w, http.StatusInternalServerError, "internal_error")
			return
		}
	}
	if lifecycle, ok := s.Store.(LifecycleStore); ok {
		events, err := lifecycle.Latest(r.Context(), s.UserID(r), home)
		if err != nil {
			slog.Error("recommendation lifecycle read failed", "error", err)
			fail(w, http.StatusInternalServerError, "internal_error")
			return
		}
		actions = ApplyLifecycle(actions, events, now)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Actions       []Action            `json:"actions"`
		AsOf          string              `json:"as_of"`
		ResearchFacts []datawizfacts.Fact `json:"research_facts,omitempty"`
	}{actions, now.Format(time.RFC3339), research})
}

func (s Service) record(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if _, err := uuid.Parse(key); err != nil {
		fail(w, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	store, ok := s.Store.(LifecycleStore)
	if !ok {
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	var event LifecycleEvent
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := uuid.Parse(event.ApplianceID); err != nil || event.RuleID == "" || len(event.RuleID) > 100 || event.RuleVersion < 1 || !validTransition("", event.State) && event.State != "completed" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	now := time.Now().UTC()
	if event.State == "snoozed" {
		if event.SnoozedUntil == nil || !event.SnoozedUntil.After(now) || event.SnoozedUntil.After(now.Add(90*24*time.Hour)) {
			fail(w, http.StatusBadRequest, "invalid_snooze")
			return
		}
	} else if event.SnoozedUntil != nil {
		fail(w, http.StatusBadRequest, "invalid_snooze")
		return
	}
	if prior, err := store.Replay(r.Context(), s.UserID(r), home, key); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		slog.Error("recommendation retry read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	} else if prior != nil {
		if prior.ApplianceID != event.ApplianceID || prior.RuleID != event.RuleID || prior.RuleVersion != event.RuleVersion || prior.State != event.State || !sameTime(prior.SnoozedUntil, event.SnoozedUntil) {
			fail(w, http.StatusConflict, "idempotency_conflict")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(prior)
		return
	}
	appliances, err := s.Store.Current(r.Context(), s.UserID(r), home, now)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("recommendation evidence read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	eligible := false
	for _, appliance := range appliances {
		if appliance.ID != event.ApplianceID {
			continue
		}
		var actions []Action
		if r.URL.Query().Get("capability") == "gas_dryer_recommendations_v1" {
			actions = EvaluateGasEnabled(appliance)
		} else {
			actions = Evaluate(appliance)
		}
		for _, action := range actions {
			if action.RuleID == event.RuleID && action.RuleVersion == event.RuleVersion && (action.State == "active" || (action.State == "candidate" && (event.State == "snoozed" || event.State == "dismissed"))) {
				eligible = true
			}
		}
	}
	if !eligible {
		fail(w, http.StatusConflict, "recommendation_not_active")
		return
	}
	result, err := store.Record(r.Context(), s.UserID(r), home, key, event)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidTransition) {
		fail(w, http.StatusConflict, "invalid_transition")
		return
	}
	if err != nil {
		slog.Error("recommendation lifecycle write failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// evaluatorFor maps the client capability to its rule vocabulary; an unknown
// or missing capability keeps the legacy rules.
func evaluatorFor(capability string) func(Appliance) []Action {
	switch capability {
	case "gas_appliance_recommendations_v1":
		return EvaluateGasAppliances
	case "gas_dryer_recommendations_v1":
		return EvaluateGasEnabled
	}
	return Evaluate
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
