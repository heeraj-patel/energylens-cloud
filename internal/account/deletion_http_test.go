package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type deletionRouteStore struct {
	routeStore
	deleted bool
	owner   string
}

func (s *deletionRouteStore) Resolve(_ context.Context, id Identity) (User, error) {
	if s.deleted && id.Subject == s.owner {
		return User{}, ErrDeleted
	}
	return User{ID: id.Subject}, nil
}

func (s *deletionRouteStore) DeleteAccount(_ context.Context, owner string) error {
	if s.deleted || owner != s.owner {
		return ErrDeleted
	}
	s.deleted = true
	return nil
}

type deletionRouteVerifier struct{ authTime time.Time }

func (v deletionRouteVerifier) Verify(_ context.Context, raw string) (Identity, error) {
	return Identity{Issuer: "https://issuer.example", Subject: raw, AuthTime: v.authTime}, nil
}

func TestDeleteMeRequiresRecentSignInAndDeniesBearerAfterCommit(t *testing.T) {
	store := &deletionRouteStore{owner: "owner"}
	verifier := deletionRouteVerifier{authTime: time.Now().Add(-6 * time.Minute)}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) { (Service{Store: store, Deletion: store, Verifier: verifier}).Routes(r) })
	call := func(actor string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/v1/me", nil)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+actor)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := call(""); w.Code != 401 {
		t.Fatalf("anonymous deletion = %d", w.Code)
	}
	if w := call("owner"); w.Code != 403 || store.deleted {
		t.Fatalf("stale sign-in = %d, deleted=%v", w.Code, store.deleted)
	}
	verifier.authTime = time.Now()
	// Rebuild the router because verifier is held by value.
	r = chi.NewRouter()
	r.Route("/v1", func(r chi.Router) { (Service{Store: store, Deletion: store, Verifier: verifier}).Routes(r) })
	if w := call("other"); w.Code != 403 || store.deleted {
		t.Fatalf("foreign owner = %d, deleted=%v", w.Code, store.deleted)
	}
	if w := call("owner"); w.Code != 204 || !store.deleted || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner deletion = %d, deleted=%v", w.Code, store.deleted)
	}
	if w := call("owner"); w.Code != 403 {
		t.Fatalf("deleted bearer = %d", w.Code)
	}
}
