package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type preferenceRouteStore struct {
	home  string
	calls int
}

func (s *preferenceRouteStore) GetAccountPreferences(_ context.Context, owner string) (AccountPreferences, error) {
	if owner != "owner" {
		return AccountPreferences{}, ErrNotFound
	}
	return AccountPreferences{Version: 1, SchemaVersion: 1}, nil
}
func (s *preferenceRouteStore) PatchAccountPreferences(_ context.Context, owner string, _ AccountPreferencesPatch, version int64, _ string, _ []byte) (AccountPreferences, error) {
	s.calls++
	if owner != "owner" {
		return AccountPreferences{}, ErrNotFound
	}
	if version != 1 {
		return AccountPreferences{}, ErrVersion
	}
	return AccountPreferences{Version: 2, SchemaVersion: 1}, nil
}
func (s *preferenceRouteStore) GetHomePreferences(_ context.Context, owner, home string) (HomePreferences, error) {
	if owner != "owner" || home != s.home {
		return HomePreferences{}, ErrNotFound
	}
	return HomePreferences{Version: 1, SchemaVersion: 1, Goal: "unspecified", ComfortPriority: "unknown"}, nil
}
func (s *preferenceRouteStore) PatchHomePreferences(_ context.Context, owner, home string, _ HomePreferencesPatch, version int64, _ string, _ []byte) (HomePreferences, error) {
	s.calls++
	if owner != "owner" || home != s.home {
		return HomePreferences{}, ErrNotFound
	}
	if version != 1 {
		return HomePreferences{}, ErrVersion
	}
	return HomePreferences{Version: 2, SchemaVersion: 1, Goal: "reduce_cost", ComfortPriority: "unknown"}, nil
}

func TestPreferenceRoutesAuthAndValidation(t *testing.T) {
	store := &preferenceRouteStore{home: uuid.NewString()}
	router := chi.NewRouter()
	router.Route("/v1", func(r chi.Router) {
		(Service{Store: routeStore{}, Verifier: fixedVerifier{}, Preferences: store}).Routes(r)
	})
	call := func(method, path, actor, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	account := "/v1/me/preferences"
	home := "/v1/homes/" + store.home + "/preferences"
	if w := call(http.MethodGet, account, "", "", nil); w.Code != 401 {
		t.Fatalf("unauthenticated preferences = %d", w.Code)
	}
	if w := call(http.MethodGet, account, "owner", "", nil); w.Code != 200 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("account preferences = %d %s", w.Code, w.Header().Get("ETag"))
	}
	if w := call(http.MethodGet, home, "other", "", nil); w.Code != 404 {
		t.Fatalf("cross-owner home preferences = %d", w.Code)
	}
	if w := call(http.MethodGet, home, "owner", "", nil); w.Code != 200 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("home preferences = %d %s", w.Code, w.Header().Get("ETag"))
	}
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(), "If-Match": `"1"`}
	for _, body := range []string{`{}`, `{"goal":null}`, `{"goal":"invalid"}`, `{"goal":"reduce_cost","unknown":true}`} {
		if w := call(http.MethodPatch, home, "owner", body, headers); w.Code != 400 {
			t.Fatalf("invalid home patch %s = %d", body, w.Code)
		}
	}
	for _, body := range []string{`{}`, `{"email_reports_enabled":null}`, `{"history_disclosure_accepted_at":"invalid"}`, `{"email_reports_enabled":true,"unknown":true}`} {
		if w := call(http.MethodPatch, account, "owner", body, headers); w.Code != 400 {
			t.Fatalf("invalid account patch %s = %d", body, w.Code)
		}
	}
	if store.calls != 0 {
		t.Fatalf("invalid requests reached store %d times", store.calls)
	}
	if w := call(http.MethodPatch, home, "other", `{"goal":"reduce_cost"}`, headers); w.Code != 404 {
		t.Fatalf("cross-owner home patch = %d", w.Code)
	}
	if w := call(http.MethodPatch, home, "owner", `{"goal":"reduce_cost"}`, headers); w.Code != 200 || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("home patch = %d %s", w.Code, w.Body.String())
	}
	delete(headers, "If-Match")
	if w := call(http.MethodPatch, account, "owner", `{"email_reports_enabled":true}`, headers); w.Code != 428 {
		t.Fatalf("missing If-Match = %d", w.Code)
	}
	headers["If-Match"] = `"2"`
	if w := call(http.MethodPatch, account, "owner", `{"email_reports_enabled":true}`, headers); w.Code != 409 {
		t.Fatalf("stale account preference version = %d", w.Code)
	}
}
