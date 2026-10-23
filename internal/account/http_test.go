package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type fixedVerifier struct{}

func (fixedVerifier) Verify(_ context.Context, raw string) (Identity, error) {
	return Identity{Issuer: "https://issuer.example/", Subject: raw}, nil
}

type routeStore struct{ home Home }

func (s routeStore) Resolve(_ context.Context, id Identity) (User, error) {
	return User{ID: id.Subject}, nil
}
func (s routeStore) List(_ context.Context, owner string) ([]Home, error) {
	if owner == "owner" {
		return []Home{s.home}, nil
	}
	return []Home{}, nil
}
func (s routeStore) Get(_ context.Context, owner, id string) (Home, error) {
	if owner == "owner" && id == s.home.ID {
		return s.home, nil
	}
	return Home{}, ErrNotFound
}
func (s routeStore) Create(_ context.Context, _ string, _ HomeCreate, _ string, _ []byte) (Home, error) {
	return s.home, nil
}
func (s routeStore) Patch(_ context.Context, owner, id string, _ HomePatch, version int64, _ string, _ []byte) (Home, error) {
	if owner != "owner" || id != s.home.ID {
		return Home{}, ErrNotFound
	}
	if version != s.home.Version {
		return Home{}, ErrVersion
	}
	h := s.home
	h.Version++
	return h, nil
}

func TestRoutesRequireBearerAndHideOtherOwners(t *testing.T) {
	h := Home{ID: uuid.NewString(), Name: "House", Timezone: "America/Chicago", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(), Location: Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) { (Service{Store: routeStore{home: h}, Verifier: fixedVerifier{}}).Routes(r) })
	call := func(method, path, actor, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := call("GET", "/v1/me", "", "", nil); w.Code != 401 {
		t.Fatalf("missing bearer = %d", w.Code)
	}
	if w := call("GET", "/v1/me", "owner", "", nil); w.Code != 200 {
		t.Fatalf("me = %d", w.Code)
	}
	if w := call("GET", "/v1/homes", "other", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"homes":[]`) {
		t.Fatalf("other list = %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/v1/homes/"+h.ID, "other", "", nil); w.Code != 404 {
		t.Fatalf("other get = %d", w.Code)
	}
	if w := call("GET", "/v1/homes/"+h.ID, "owner", "", nil); w.Code != 200 || w.Header().Get("ETag") != `"1"` || !strings.Contains(w.Body.String(), `"first_device_added_at":null`) {
		t.Fatalf("owner get = %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/v1/homes", "owner", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"first_device_added_at":null`) {
		t.Fatalf("owner list missing incomplete state: %d %s", w.Code, w.Body.String())
	}
	patch := `{"name":"Renamed"}`
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(), "If-Match": `"1"`}
	if w := call("PATCH", "/v1/homes/"+h.ID, "owner", `{"first_device_added_at":"2026-10-22T00:00:00Z"}`, headers); w.Code != 400 {
		t.Fatalf("client milestone write accepted: %d %s", w.Code, w.Body.String())
	}
	if w := call("PATCH", "/v1/homes/"+h.ID, "other", patch, headers); w.Code != 404 {
		t.Fatalf("other patch = %d", w.Code)
	}
	headers["If-Match"] = `"2"`
	if w := call("PATCH", "/v1/homes/"+h.ID, "owner", patch, headers); w.Code != 409 {
		t.Fatalf("stale patch = %d", w.Code)
	}
	headers["If-Match"] = `"1"`
	if w := call("PATCH", "/v1/homes/"+h.ID, "owner", patch, headers); w.Code != 200 {
		t.Fatalf("owner patch = %d %s", w.Code, w.Body.String())
	}
	var result Home
	if err := json.Unmarshal([]byte(call("GET", "/v1/homes/"+h.ID, "owner", "", nil).Body.String()), &result); err != nil {
		t.Fatal(err)
	}
}
