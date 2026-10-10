package account

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

type profileRouteStore struct{ routeStore }

func (s profileRouteStore) Resolve(_ context.Context, id Identity) (User, error) {
	return User{ID: id.Subject, ProfileVersion: 1, CreatedAt: time.Now()}, nil
}

type recordingProfileStore struct {
	owner string
	name  *string
}

func (s *recordingProfileStore) PatchProfile(_ context.Context, owner string, name *string, version int64, _ string, _ []byte) (User, error) {
	s.owner, s.name = owner, name
	if version != 1 {
		return User{}, ErrVersion
	}
	return User{ID: owner, DisplayName: name, ProfileVersion: 2, CreatedAt: time.Now()}, nil
}

func TestProfileRouteValidationAndOwnerScope(t *testing.T) {
	profile := &recordingProfileStore{}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		(Service{Store: profileRouteStore{}, Profile: profile, Verifier: fixedVerifier{}}).Routes(r)
	})
	call := func(actor, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/v1/me/profile", strings.NewReader(body))
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
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(), "If-Match": `"1"`}
	if got := call("", `{"display_name":"Ada"}`, headers); got.Code != 401 {
		t.Fatalf("unauthenticated patch = %d", got.Code)
	}
	for _, body := range []string{`{}`, `{"display_name":""}`, `{"display_name":" Ada"}`, `{"display_name":42}`, `{"display_name":"Ada","email":"other@example.com"}`} {
		if got := call("owner", body, headers); got.Code != 400 {
			t.Fatalf("invalid %s = %d", body, got.Code)
		}
	}
	if got := call("owner", `{"display_name":"Ada"}`, headers); got.Code != 200 || got.Header().Get("ETag") != `"2"` || profile.owner != "owner" || profile.name == nil || *profile.name != "Ada" {
		t.Fatalf("profile patch = %d %s", got.Code, got.Body.String())
	}
	if got := call("another", `{"display_name":null}`, headers); got.Code != 200 || profile.owner != "another" || profile.name != nil {
		t.Fatalf("clear another account = %d %s", got.Code, got.Body.String())
	}
	headers["If-Match"] = `"3"`
	if got := call("owner", `{"display_name":"Ada"}`, headers); got.Code != 409 {
		t.Fatalf("stale profile patch = %d", got.Code)
	}
}
