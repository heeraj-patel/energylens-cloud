package recommendation

import (
	"encoding/json"
	"errors"
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
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/recommendations", s.list)
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
	gasEnabled := r.URL.Query().Get("capability") == "gas_dryer_recommendations_v1"
	for _, appliance := range appliances {
		if gasEnabled {
			actions = append(actions, EvaluateGasEnabled(appliance)...)
		} else {
			actions = append(actions, Evaluate(appliance)...)
		}
	}
	actions = rankActions(actions)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Actions []Action `json:"actions"`
		AsOf    string   `json:"as_of"`
	}{actions, now.Format(time.RFC3339)})
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
