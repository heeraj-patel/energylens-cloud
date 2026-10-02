package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type stubStore struct {
	appliances  []Appliance
	err         error
	owner, home string
}

func (s *stubStore) Current(_ context.Context, owner, home string, _ time.Time) ([]Appliance, error) {
	s.owner, s.home = owner, home
	return s.appliances, s.err
}

func TestRecommendationsRouteUsesOwnerAndNeverPromisesSavings(t *testing.T) {
	home, device := uuid.NewString(), uuid.NewString()
	store := &stubStore{appliances: []Appliance{{ID: device, Category: "refrigerator", Fuel: "electricity", Identity: "confirmed_exact",
		Facts:  []Evidence{{ID: uuid.NewString(), Kind: "annual_energy", Revision: 1}},
		Tariff: &Evidence{ID: uuid.NewString(), Kind: "tariff", Revision: 2}}}}
	r := chi.NewRouter()
	Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}.Routes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusOK || store.owner != "owner" || store.home != home {
		t.Fatalf("response %d, owner %q, home %q", w.Code, store.owner, store.home)
	}
	var body struct {
		Actions []Action `json:"actions"`
		AsOf    string   `json:"as_of"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Actions) != 1 || body.Actions[0].State != "active" || len(body.Actions[0].EvidenceRefs) != 2 || body.Actions[0].SavingsAmount != nil || body.AsOf == "" {
		t.Fatalf("unbounded action: %+v", body)
	}
	store.err = ErrNotFound
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign home: %d", w.Code)
	}
	store.err = errors.New("storage unavailable")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homes/"+home+"/recommendations", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure: %d", w.Code)
	}
}
