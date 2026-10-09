package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/householdenergy"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// The shared-contract test consumes this marker from the actual HTTP encoder.
func TestDemoApplianceSerializedContract(t *testing.T) {
	home := uuid.NewString()
	demo := Appliance{ID: uuid.NewString(), HomeID: home, Name: "Named demo", DemoDeviceID: "demo-device-01", DemoFixtureRevision: "demo-revision", Identity: Identity{MatchQuality: "unresolved"}, Version: 1}
	store := &routeStore{a: Appliance{ID: uuid.NewString(), HomeID: home}, empty: true, extra: []Appliance{demo}}
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return "owner" }}
	router := chi.NewRouter()
	service.Routes(router)
	for _, path := range []string{"/homes/" + home + "/appliances/", "/homes/" + home + "/appliances/" + demo.ID} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" {
			t.Fatalf("demo GET %s = %d", path, w.Code)
		}
		var payload any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload)
		fmt.Println("DEMO_APPLIANCE_JSON:" + string(encoded))
	}
}

type routeStore struct {
	a       Appliance
	writes  int
	empty   bool
	listErr error
	getErr  error
	extra   []Appliance
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
		return append([]Appliance{}, s.extra...), nil
	}
	return append([]Appliance{s.a}, s.extra...), nil
}

func (s *routeStore) ListWithRevision(ctx context.Context, owner, home string) ([]Appliance, string, error) {
	a, err := s.List(ctx, owner, home)
	if err != nil {
		return nil, "", err
	}
	return a, inventoryRevision("persisted", home, a), nil
}

func TestEmptyInventoryDoesNotUseSampleFixture(t *testing.T) {
	home := uuid.NewString()
	store := &routeStore{a: Appliance{ID: uuid.NewString(), HomeID: home}, empty: true}
	service := Service{Store: store, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }}
	router := chi.NewRouter()
	service.Routes(router)
	path := "/homes/" + home + "/appliances/"
	call := func(method, actor, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call(http.MethodGet, "owner", path); w.Code != http.StatusOK || w.Header().Get(InventoryRevisionHeader) == "" || w.Header().Get("X-EnergyLens-Data-Mode") != "" || !strings.Contains(w.Body.String(), `"appliances":[]`) {
		t.Fatalf("empty inventory = %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodGet, "owner", path+uuid.NewString()); w.Code != http.StatusNotFound || w.Header().Get("X-EnergyLens-Data-Mode") != "" {
		t.Fatalf("missing appliance = %d %s", w.Code, w.Body.String())
	}
	demo := Appliance{ID: uuid.NewString(), HomeID: home, Name: "Named demo", DemoDeviceID: "demo-device-01", DemoFixtureRevision: "demo-revision", Identity: Identity{MatchQuality: "unresolved"}, Version: 1}
	store.extra = []Appliance{demo}
	w := call(http.MethodGet, "owner", path)
	if w.Code != http.StatusOK || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" || w.Header().Get("X-EnergyLens-Fixture-Revision") != "demo-revision" || !strings.Contains(w.Body.String(), `"demo_device_id":"demo-device-01"`) {
		t.Fatalf("snapshot inventory envelope = %d %s", w.Code, w.Body.String())
	}
	w = call(http.MethodGet, "owner", path+demo.ID)
	if w.Code != http.StatusOK || w.Header().Get("X-EnergyLens-Data-Mode") != "sample" || !strings.Contains(w.Body.String(), `"demo_device_id":"demo-device-01"`) {
		t.Fatalf("snapshot readback = %d %s", w.Code, w.Body.String())
	}
}

func TestDemoAppliancesExposeOnlyNamedSnapshotLeaves(t *testing.T) {
	snapshot := householdenergy.DemoCostSnapshot{Version: 1, FixtureRevision: "cost_snapshot_v1", Timezone: "America/Chicago", ReferenceLocalDate: "2026-09-28", Currency: "USD", Provenance: "synthetic", Devices: make([]householdenergy.DemoSnapshotDevice, 18)}
	names := []string{"Study Desktop & Rig", `Living Room 65" TV`, "Phone & Tablet Chargers", "Kitchen Refrigerator", "Hallway Central AC", "Living Room Soundbar", "Home Office Monitors", "Mesh Wi-Fi Gateway & Router", "Subwoofer & Amp Rack", "Central Heat Pump"}
	for i := range snapshot.Devices {
		id := fmt.Sprintf("demo-device-%02d", i+1)
		var deviceName *string
		if i >= 10 {
			deviceName = nil
		} else {
			deviceName = &names[i]
		}
		snapshot.Devices[i] = householdenergy.DemoSnapshotDevice{ID: id, Name: deviceName}
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	home := uuid.NewString()
	rows, err := demoAppliances(snapshot, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("named rows = %d", len(rows))
	}
	again, err := demoAppliances(snapshot, home)
	if err != nil {
		t.Fatal(err)
	}
	otherHomeRows, err := demoAppliances(snapshot, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if row.DemoDeviceID != fmt.Sprintf("demo-device-%02d", i+1) || row.DemoFixtureRevision != snapshot.FixtureRevision || row.Name != names[i] || row.HomeID != home || row.ID != again[i].ID || row.ID == otherHomeRows[i].ID || row.Category != "" || row.Identity.MatchQuality != "unresolved" || row.Identity.Brand != "" || row.Identity.FullModel != "" || row.Identity.Fuel != "" {
			t.Fatalf("unexpected demo row %+v", row)
		}
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
