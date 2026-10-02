package tariff

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
	calls int
	in    Create
}

func (s *routeStore) List(_ context.Context, owner, home string) ([]Tariff, error) {
	s.calls++
	if owner != "owner" {
		return nil, ErrNotFound
	}
	return []Tariff{}, nil
}
func (s *routeStore) Create(_ context.Context, owner, home string, in Create, _ string, _ []byte) (Tariff, error) {
	s.calls++
	s.in = in
	if owner != "owner" {
		return Tariff{}, ErrNotFound
	}
	return Tariff{ID: uuid.NewString(), HomeID: home, Revision: 1, Create: in}, nil
}
func TestTariffHTTPAuthAndValidation(t *testing.T) {
	s := &routeStore{}
	service := Service{Store: s, Auth: func(next http.Handler) http.Handler {
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
	home := uuid.NewString()
	path := "/v1/homes/" + home + "/tariffs"
	call := func(method, body, actor, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := call("GET", "", "", ""); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	if w := call("GET", "", "other", ""); w.Code != 404 {
		t.Fatalf("other owner: %d", w.Code)
	}
	if w := call("GET", "", "owner", ""); w.Code != 200 || w.Body.String() != "{\"tariffs\":[]}\n" {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	now := time.Now().UTC().Format(time.RFC3339)
	body := `{"fuel":"electricity","rate":"0.1588","rate_unit":"kWh","currency":"USD","rate_kind":"user_tariff","source_ref":"bill","source_as_of":"` + now + `","is_fallback":false,"fixed_charge":"12.50","effective_from":"` + now + `","effective_to":null}`
	before := s.calls
	for _, invalid := range []string{`{}`, strings.Replace(body, `"fixed_charge":"12.50",`, "", 1), strings.Replace(body, `"rate_unit":"kWh"`, `"rate_unit":"therm"`, 1), strings.Replace(body, `"rate":"0.1588"`, `"rate":0.1588`, 1)} {
		if w := call("POST", invalid, "owner", uuid.NewString()); w.Code != 400 {
			t.Fatalf("invalid %q: %d %s", invalid, w.Code, w.Body.String())
		}
	}
	if w := call("POST", body, "owner", ""); w.Code != 400 {
		t.Fatalf("missing key: %d", w.Code)
	}
	if s.calls != before {
		t.Fatalf("invalid requests reached store: %d", s.calls-before)
	}
	if w := call("POST", body, "owner", uuid.NewString()); w.Code != 201 || !strings.Contains(w.Body.String(), `"fixed_charge":"12.50"`) {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
}
