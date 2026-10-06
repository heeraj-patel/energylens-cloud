package inventory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type routeStore struct {
	a      Appliance
	writes int
}

func TestDisclosureRequiredErrorCode(t *testing.T) {
	w := httptest.NewRecorder()
	storeError(w, ErrDisclosureRequired)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"history_disclosure_required"`) {
		t.Fatalf("disclosure response = %d %s", w.Code, w.Body.String())
	}
}

func (s *routeStore) List(_ context.Context, owner, home string) ([]Appliance, error) {
	if owner != "owner" || home != s.a.HomeID {
		return nil, ErrNotFound
	}
	return []Appliance{s.a}, nil
}
func (s *routeStore) Get(_ context.Context, owner, home, id string) (Appliance, error) {
	if owner != "owner" || home != s.a.HomeID || id != s.a.ID {
		return Appliance{}, ErrNotFound
	}
	return s.a, nil
}
func (s *routeStore) Create(_ context.Context, owner, home string, _ CreateInput, _ string, _ []byte) (Appliance, error) {
	s.writes++
	if owner != "owner" || home != s.a.HomeID {
		return Appliance{}, ErrNotFound
	}
	return s.a, nil
}
func (s *routeStore) Patch(_ context.Context, owner, home, id string, _ PatchInput, version int64, _ string, _ []byte) (Appliance, error) {
	s.writes++
	if owner != "owner" || home != s.a.HomeID || id != s.a.ID {
		return Appliance{}, ErrNotFound
	}
	if version != 1 {
		return Appliance{}, ErrVersion
	}
	a := s.a
	a.Version = 2
	return a, nil
}
func (s *routeStore) Archive(_ context.Context, owner, home, id string, version int64, _ string, _ []byte) (Appliance, error) {
	s.writes++
	if owner != "owner" || home != s.a.HomeID || id != s.a.ID {
		return Appliance{}, ErrNotFound
	}
	if version != s.a.Version {
		return Appliance{}, ErrVersion
	}
	a := s.a
	a.Version++
	return a, nil
}
func TestInventoryRoutesAuthValidationAndETag(t *testing.T) {
	store := &routeStore{a: Appliance{ID: uuid.NewString(), HomeID: uuid.NewString(), Name: "Fridge", Category: "refrigerator", Identity: Identity{Brand: "Acme", FullModel: "A123", Fuel: "electricity", MatchQuality: "unresolved"}, Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer owner" && r.Header.Get("Authorization") != "Bearer other" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }}
	router := chi.NewRouter()
	router.Route("/v1", service.Routes)
	base := "/v1/homes/" + store.a.HomeID + "/appliances"
	call := func(method, path, actor, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if w := call("GET", base, "", "", nil); w.Code != 401 {
		t.Fatalf("missing auth = %d", w.Code)
	}
	if w := call("GET", base, "other", "", nil); w.Code != 404 {
		t.Fatalf("cross-owner list = %d", w.Code)
	}
	if w := call("GET", base, "owner", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"appliances"`) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", base+"/"+store.a.ID, "owner", "", nil); w.Code != 200 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("get = %d %s", w.Code, w.Header().Get("ETag"))
	}
	create := `{"name":"Fridge","category":"refrigerator","identity":{"brand":"Acme","full_model":"A123","fuel":"electricity","match_quality":"unresolved"}}`
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": uuid.NewString()}
	if w := call("POST", base, "owner", create, headers); w.Code != 201 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	headers["If-Match"] = `"1"`
	for _, body := range []string{`{}`, `{"name":null}`, `{"room":" "}`, `{"identity":null}`, `{"unknown":1}`} {
		if w := call("PATCH", base+"/"+store.a.ID, "owner", body, headers); w.Code != 400 {
			t.Fatalf("invalid patch %s = %d", body, w.Code)
		}
	}
	if store.writes != 1 {
		t.Fatalf("invalid patches reached store %d times", store.writes)
	}
	if w := call("PATCH", base+"/"+store.a.ID, "owner", `{"name":"New"}`, headers); w.Code != 200 || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("patch = %d %s", w.Code, w.Body.String())
	}
	if w := call("PATCH", base+"/"+store.a.ID, "owner", `{"room":null}`, headers); w.Code != 200 {
		t.Fatalf("clear room patch = %d %s", w.Code, w.Body.String())
	}
	delete(headers, "If-Match")
	if w := call("PATCH", base+"/"+store.a.ID, "owner", `{"name":"New"}`, headers); w.Code != 428 {
		t.Fatalf("missing If-Match = %d", w.Code)
	}
	deleteURL := base + "/" + store.a.ID
	if w := call("DELETE", deleteURL, "", "", headers); w.Code != 401 {
		t.Fatalf("unauthenticated archive = %d", w.Code)
	}
	if w := call("DELETE", deleteURL, "owner", "", headers); w.Code != 428 {
		t.Fatalf("missing archive If-Match = %d", w.Code)
	}
	headers["If-Match"] = `"1"`
	if w := call("DELETE", deleteURL, "other", "", headers); w.Code != 404 {
		t.Fatalf("cross-owner archive = %d", w.Code)
	}
	if w := call("DELETE", deleteURL, "owner", `{"force":true}`, headers); w.Code != 400 {
		t.Fatalf("archive body = %d", w.Code)
	}
	if w := call("DELETE", deleteURL, "owner", "", headers); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("archive = %d %s", w.Code, w.Body.String())
	}
}

func TestInventoryRoutesCoexistWithHomeRoutes(t *testing.T) {
	router := chi.NewRouter()
	router.Route("/v1", func(v chi.Router) {
		v.Route("/homes", func(h chi.Router) {
			h.Get("/{home}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
		})
		(Service{Store: &routeStore{}, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}).Routes(v)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/homes/"+uuid.NewString()+"/appliances", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 404 || !strings.Contains(response.Body.String(), `"not_found"`) {
		t.Fatalf("inventory route lost to home route: %d %s", response.Code, response.Body.String())
	}
}
