package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type routeStore struct {
	a            Appliance
	writes       int
	empty        bool
	history      bool
	listErr      error
	historyErr   error
	getErr       error
	collision    bool
	collisionErr error
}

func (s *routeStore) HasApplianceID(_ context.Context, _ string) (bool, error) {
	return s.collision, s.collisionErr
}

func (s *routeStore) HasInventoryHistory(_ context.Context, owner, home string) (bool, error) {
	if s.historyErr != nil {
		return false, s.historyErr
	}
	return owner == "owner" && home == s.a.HomeID && (s.history || s.a.ID != "" && !s.empty), nil
}

func TestDisclosureRequiredErrorCode(t *testing.T) {
	w := httptest.NewRecorder()
	storeError(w, ErrDisclosureRequired)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"history_disclosure_required"`) {
		t.Fatalf("disclosure response = %d %s", w.Code, w.Body.String())
	}
}

func (s *routeStore) List(_ context.Context, owner, home string) ([]Appliance, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if owner != "owner" || home != s.a.HomeID {
		return nil, ErrNotFound
	}
	if s.empty {
		return []Appliance{}, nil
	}
	return []Appliance{s.a}, nil
}

func TestApplianceSampleSameRouteAndRealDataPrecedence(t *testing.T) {
	home := uuid.NewString()
	users := map[string]bool{"owner": true}
	homes := map[string]bool{home: true}
	sample, err := NewSampleProvider(users, homes)
	if err != nil {
		t.Fatal(err)
	}
	store := &routeStore{a: Appliance{ID: uuid.NewString(), HomeID: home, Name: "Actual", Version: 1}, empty: true}
	service := Service{Store: store, Sample: sample, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }}
	router := chi.NewRouter()
	router.Route("/v1", service.Routes)
	call := func(actor, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/v1/homes/" + home + "/appliances"
	if w := call("", path); w.Code != 401 {
		t.Fatalf("unauthenticated = %d", w.Code)
	}
	if w := call("other", path); w.Code != 404 {
		t.Fatalf("cross-owner = %d", w.Code)
	}
	w := call("owner", path)
	if w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" || w.Header().Get("X-EnergyLens-Fixture-Revision") != SampleRevision || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sample response = %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["appliances"] == nil {
		t.Fatalf("response shape = %s", w.Body.String())
	}
	var items []Appliance
	if err := json.Unmarshal(body["appliances"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("sample items = %d", len(items))
	}
	for _, a := range items {
		if a.HomeID != home || a.Version != 1 || a.Identity.MatchQuality != "unresolved" {
			t.Fatalf("bad sample row %+v", a)
		}
	}
	delete(sample.AllowedUsers, "owner")
	w = call("owner", path)
	if w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"appliances":[]`) {
		t.Fatalf("user allowlist = %d %s", w.Code, w.Body.String())
	}
	sample.AllowedUsers["owner"] = true
	delete(sample.AllowedHomes, home)
	w = call("owner", path)
	if w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"appliances":[]`) {
		t.Fatalf("home allowlist = %d %s", w.Code, w.Body.String())
	}
	sample.AllowedHomes[home] = true
	store.empty = false
	w = call("owner", path)
	if w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), "Actual") {
		t.Fatalf("real inventory = %d %s", w.Code, w.Body.String())
	}
	store.empty, store.history = true, true
	w = call("owner", path)
	if w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"appliances":[]`) {
		t.Fatalf("archived inventory = %d %s", w.Code, w.Body.String())
	}
	store.history = false
	store.listErr = errors.New("db down")
	if w = call("owner", path); w.Code != 500 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("list error = %d", w.Code)
	}
	store.listErr, store.historyErr = nil, errors.New("db down")
	if w = call("owner", path); w.Code != 500 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("history error = %d", w.Code)
	}
	store.historyErr = nil
	service.Sample = nil
	router = chi.NewRouter()
	router.Route("/v1", service.Routes)
	if w = call("owner", path); w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"appliances":[]`) {
		t.Fatalf("disabled sample = %d %s", w.Code, w.Body.String())
	}
}
func (s *routeStore) Get(_ context.Context, owner, home, id string) (Appliance, error) {
	if s.getErr != nil {
		return Appliance{}, s.getErr
	}
	if owner != "owner" || home != s.a.HomeID || id != s.a.ID {
		return Appliance{}, ErrNotFound
	}
	return s.a, nil
}

func TestApplianceSampleDetailOwnershipPrecedenceAndMutation(t *testing.T) {
	home := uuid.NewString()
	sample, err := NewSampleProvider(map[string]bool{"owner": true}, map[string]bool{home: true})
	if err != nil {
		t.Fatal(err)
	}
	fixture := sample.Appliances(home)[0]
	store := &routeStore{a: Appliance{ID: uuid.NewString(), HomeID: home, Name: "Actual", Version: 1}, empty: true}
	service := Service{Store: store, Sample: sample, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }}
	router := chi.NewRouter()
	router.Route("/v1", service.Routes)
	path := "/v1/homes/" + home + "/appliances/" + fixture.ID
	call := func(method, actor, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		if method != http.MethodGet {
			req.Header.Set("Idempotency-Key", uuid.NewString())
			req.Header.Set("If-Match", `"1"`)
			if method == http.MethodPatch {
				req.Body = io.NopCloser(strings.NewReader(`{"name":"Changed"}`))
				req.Header.Set("Content-Type", "application/json")
			}
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call("GET", "", path); w.Code != 401 {
		t.Fatalf("auth = %d", w.Code)
	}
	if w := call("GET", "other", path); w.Code != 404 {
		t.Fatalf("owner = %d", w.Code)
	}
	w := call("GET", "owner", path)
	if w.Code != 200 || w.Header().Get("ETag") != `"1"` || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" || w.Header().Get("X-EnergyLens-Fixture-Revision") != SampleRevision {
		t.Fatalf("sample detail = %d %s", w.Code, w.Body.String())
	}
	var got Appliance
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.ID != fixture.ID || got.HomeID != home || got.Name != fixture.Name {
		t.Fatalf("sample body = %+v %v", got, err)
	}
	if w = call("GET", "owner", "/v1/homes/"+home+"/appliances/"+uuid.NewString()); w.Code != 404 {
		t.Fatalf("nonfixture = %d", w.Code)
	}
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		if w = call(method, "owner", path); w.Code != 404 {
			t.Fatalf("sample mutation %s = %d", method, w.Code)
		}
	}
	if store.writes != 2 {
		t.Fatalf("mutations should reach real store: writes = %d", store.writes)
	}
	store.a.ID = fixture.ID
	if w = call("GET", "owner", path); w.Code != 200 || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), "Actual") {
		t.Fatalf("real ID precedence = %d %s", w.Code, w.Body.String())
	}
	if w = call("PATCH", "owner", path); w.Code != 200 || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("real patch = %d", w.Code)
	}
	if w = call("DELETE", "owner", path); w.Code != 204 {
		t.Fatalf("real delete = %d", w.Code)
	}
	store.a.ID = uuid.NewString()
	store.empty = false
	if w = call("GET", "owner", path); w.Code != 404 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("real active precedence = %d", w.Code)
	}
	store.empty, store.history = true, true
	if w = call("GET", "owner", path); w.Code != 404 {
		t.Fatalf("archive precedence = %d", w.Code)
	}
	store.history, store.collision = false, true
	if w = call("GET", "owner", path); w.Code != 404 {
		t.Fatalf("global ID collision = %d", w.Code)
	}
	store.collision = false
	for _, setErr := range []func(error){func(e error) { store.getErr = e }, func(e error) { store.listErr = e }, func(e error) { store.historyErr = e }, func(e error) { store.collisionErr = e }} {
		setErr(errors.New("db down"))
		if w = call("GET", "owner", path); w.Code != 500 || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
			t.Fatalf("store error = %d", w.Code)
		}
		setErr(nil)
	}
	delete(sample.AllowedUsers, "owner")
	if w = call("GET", "owner", path); w.Code != 404 {
		t.Fatalf("user gate = %d", w.Code)
	}
	sample.AllowedUsers["owner"] = true
	delete(sample.AllowedHomes, home)
	if w = call("GET", "owner", path); w.Code != 404 {
		t.Fatalf("home gate = %d", w.Code)
	}
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
