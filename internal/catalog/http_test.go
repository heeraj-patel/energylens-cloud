package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type fakeStore struct {
	query Search
	reads int
}

func (s *fakeStore) Search(_ context.Context, q Search) ([]json.RawMessage, error) {
	s.query = q
	s.reads++
	return []json.RawMessage{}, nil
}
func (s *fakeStore) Get(_ context.Context, _ string) (json.RawMessage, error) {
	s.reads++
	return json.RawMessage(`{"id":"` + uuid.NewString() + `","facts":[]}`), nil
}

func TestCatalogRoutesValidateAuthAndSearch(t *testing.T) {
	store := &fakeStore{}
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer owner" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}}
	router := chi.NewRouter()
	router.Route("/v1", service.Routes)
	call := func(method, path, body, token, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/v1/catalog/model-search"
	valid := `{"brand":" Acme ","model":" A123 ","fuel":"electricity"}`
	if w := call("POST", path, valid, "", uuid.NewString()); w.Code != 401 {
		t.Fatalf("unauthenticated search = %d", w.Code)
	}
	for _, body := range []string{`{"brand":"A","model":"B"}`, `{"brand":"A","model":"B12","serial":"private"}`, `{"brand":"A","model":"B12","fuel":"gas"}`, `{"brand":"A","model":"B12","fuel":null}`, `{"brand":"A","model":"B12","variant":" "}`, `{"brand":"A","model":"B12"} {}`} {
		if w := call("POST", path, body, "owner", uuid.NewString()); w.Code != 400 {
			t.Fatalf("invalid search %q = %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := call("POST", path, valid, "owner", ""); w.Code != 400 {
		t.Fatalf("missing idempotency key = %d", w.Code)
	}
	if store.reads != 0 {
		t.Fatalf("invalid requests reached store: %d", store.reads)
	}
	if w := call("POST", path, valid, "owner", uuid.NewString()); w.Code != 200 || w.Body.String() != "{\"products\":[]}\n" {
		t.Fatalf("valid search = %d %s", w.Code, w.Body.String())
	}
	if store.query.Brand != "Acme" || store.query.Model != "A123" || store.query.Fuel != "electricity" {
		t.Fatalf("normalized search = %+v", store.query)
	}
	if w := call("POST", path, `{"brand":"LG","model":"WM3070HXA","variant":" US-120V "}`, "owner", uuid.NewString()); w.Code != 200 || store.query.Variant == nil || *store.query.Variant != "US-120V" {
		t.Fatalf("variant search = %d %+v", w.Code, store.query)
	}
	if w := call("GET", "/v1/catalog/products/not-a-uuid", "", "owner", ""); w.Code != 404 {
		t.Fatalf("invalid product ID = %d", w.Code)
	}
	if w := call("GET", "/v1/catalog/products/"+uuid.NewString(), "", "owner", ""); w.Code != 200 {
		t.Fatalf("valid product ID = %d %s", w.Code, w.Body.String())
	}
}
