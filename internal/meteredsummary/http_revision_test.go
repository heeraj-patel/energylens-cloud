package meteredsummary

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type revisionTestStore struct {
	scopeErr, pageErr error
	revision          int64
	rows              []Row
	history           bool
}

func (s revisionTestStore) Scope(context.Context, Query) error { return s.scopeErr }
func (s revisionTestStore) PageAtRevisionWithHistory(context.Context, Query, *int64, bool) ([]Row, int64, bool, error) {
	return s.rows, s.revision, s.history, s.pageErr
}

func TestEvidenceRevisionHeaderOnlyOnSuccessfulResponses(t *testing.T) {
	owner, home := uuid.NewString(), uuid.NewString()
	sample, err := NewSampleProvider(map[string]bool{owner: true}, map[string]bool{home: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/homes/" + home + "/metered-summaries?granularity=day&from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z"
	serve := func(store revisionTestStore, user, requestPath string) *httptest.ResponseRecorder {
		t.Helper()
		router := chi.NewRouter()
		Service{Store: store, Sample: sample, Auth: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-User") == "" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		}, UserID: func(r *http.Request) string { return r.Header.Get("X-User") }}.Routes(router)
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		if user != "" {
			req.Header.Set("X-User", user)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		name       string
		store      revisionTestStore
		user, path string
		status     int
	}{
		{"unauthenticated", revisionTestStore{}, "", path, 401},
		{"invalid query", revisionTestStore{}, owner, "/homes/" + home + "/metered-summaries?granularity=bad", 400},
		{"ownership", revisionTestStore{scopeErr: ErrNotFound}, owner, path, 404},
		{"scope store error", revisionTestStore{scopeErr: errors.New("scope failed")}, owner, path, 500},
		{"page ownership error", revisionTestStore{pageErr: ErrNotFound}, owner, path, 404},
		{"changed evidence", revisionTestStore{pageErr: ErrRevisionChanged}, owner, path, 409},
		{"page store error", revisionTestStore{pageErr: errors.New("page failed")}, owner, path, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(tc.store, tc.user, tc.path)
			if w.Code != tc.status || w.Header().Get("X-EnergyLens-Evidence-Revision") != "" {
				t.Fatalf("response %d with revision %q", w.Code, w.Header().Get("X-EnergyLens-Evidence-Revision"))
			}
		})
	}
	for _, tc := range []struct {
		name  string
		store revisionTestStore
		want  string
	}{
		{"empty real home", revisionTestStore{revision: 0, history: true}, "v1-0"},
		{"real evidence", revisionTestStore{revision: 42, rows: []Row{{Payload: []byte(`{"kind":"energy"}`)}}}, "v1-42"},
		{"sample fixture", revisionTestStore{}, sample.evidenceRevision()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(tc.store, owner, path)
			if w.Code != 200 || w.Header().Get("X-EnergyLens-Evidence-Revision") != tc.want {
				t.Fatalf("response %d with revision %q; wanted %q", w.Code, w.Header().Get("X-EnergyLens-Evidence-Revision"), tc.want)
			}
		})
	}
	if sample.evidenceRevision() == "v1-0" {
		t.Fatal("sample token collided with real revision")
	}
}
