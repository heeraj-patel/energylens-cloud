package estimation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type routeRunStore struct {
	owner, home string
	reads       int
}

func (s *routeRunStore) ListRuns(_ context.Context, owner, home string, appliance *string, _ time.Time) ([]json.RawMessage, error) {
	s.reads++
	if owner != s.owner || home != s.home {
		return nil, ErrNotFound
	}
	return []json.RawMessage{}, nil
}
func TestEstimateRouteOwnerAndValidation(t *testing.T) {
	store := &routeRunStore{owner: "owner", home: uuid.NewString()}
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }}
	r := chi.NewRouter()
	r.Route("/v1", service.Routes)
	call := func(path, actor string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	base := "/v1/homes/" + store.home + "/estimates"
	if w := call(base, ""); w.Code != 401 {
		t.Fatalf("unauthenticated = %d", w.Code)
	}
	if w := call(base, "other"); w.Code != 404 {
		t.Fatalf("other owner = %d", w.Code)
	}
	if w := call(base+"?appliance_id=bad", "owner"); w.Code != 400 {
		t.Fatalf("invalid filter = %d", w.Code)
	}
	if w := call(base, "owner"); w.Code != 200 || !strings.Contains(w.Body.String(), `"estimates":[]`) {
		t.Fatalf("owner list = %d %s", w.Code, w.Body.String())
	}
}
