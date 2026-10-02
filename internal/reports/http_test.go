package reports

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type routeStore struct {
	created  bool
	read     bool
	queued   bool
	owner    string
	selected []uuid.UUID
	delivery DeliveryInput
	err      error
}

func (s *routeStore) Create(_ context.Context, owner, home string, ids []uuid.UUID, _ uuid.UUID, _ time.Time) (Preview, error) {
	s.created, s.owner, s.selected = true, owner, ids
	if s.err != nil {
		return Preview{}, s.err
	}
	return Preview{ID: uuid.NewString(), HomeID: home, SchemaVersion: "manual-preview/v1", Items: []Item{}}, nil
}
func (s *routeStore) Get(_ context.Context, owner, home, id string, _ time.Time) (Preview, error) {
	s.read, s.owner = true, owner
	if s.err != nil {
		return Preview{}, s.err
	}
	return Preview{ID: id, HomeID: home, SchemaVersion: "manual-preview/v1", Items: []Item{}}, nil
}
func (s *routeStore) QueueDelivery(_ context.Context, owner, home string, in DeliveryInput, _ time.Time) (DeliveryRequest, error) {
	s.queued, s.owner, s.delivery = true, owner, in
	if s.err != nil {
		return DeliveryRequest{}, s.err
	}
	return DeliveryRequest{ID: uuid.NewString(), PreviewID: in.PreviewID, Recipients: in.Recipients, State: "pending"}, nil
}

func TestDeliveryRouteRequiresExplicitApprovalAndMapsStoreErrors(t *testing.T) {
	home, preview, key := uuid.NewString(), uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("a", 64)
	store := &routeStore{}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	router := chi.NewRouter()
	Service{Store: store, Auth: auth, UserID: func(*http.Request) string { return "verified-owner" }}.Routes(router)
	path := "/homes/" + home + "/reports/previews/" + preview + "/deliveries"
	valid := `{"approved_snapshot_sha256":"` + digest + `","recipients":["one@example.com"],"approved":true}`
	call := func(path, body, token, idempotency string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotency)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call(path, valid, "", key); w.Code != 401 || store.queued {
		t.Fatalf("unauthenticated enqueue: %d", w.Code)
	}
	for _, body := range []string{
		`{}`, `{"approved_snapshot_sha256":"` + digest + `","recipients":["one@example.com"]}`,
		`{"approved_snapshot_sha256":"bad","recipients":["one@example.com"],"approved":true}`,
		`{"approved_snapshot_sha256":"` + strings.ToUpper(digest) + `","recipients":["one@example.com"],"approved":true}`,
		`{"approved_snapshot_sha256":"` + digest + `","recipients":[],"approved":true}`,
		`{"approved_snapshot_sha256":"` + digest + `","recipients":["one@example.com","one@example.com"],"approved":true}`,
		`{"approved_snapshot_sha256":"` + digest + `","recipients":["one@example.com","two@example.com","three@example.com","four@example.com","five@example.com","six@example.com"],"approved":true}`,
		`{"approved_snapshot_sha256":"` + digest + `","recipients":["bad address"],"approved":true}`,
		valid + `{}`, `{"approved_snapshot_sha256":"` + digest + `","recipients":["one@example.com"],"approved":true,"provider":"x"}`,
		strings.Repeat(" ", 4097) + valid,
	} {
		if w := call(path, body, "Bearer valid", key); w.Code != 400 && w.Code != 413 {
			t.Fatalf("bad request %d: %s", w.Code, body)
		}
	}
	if store.queued {
		t.Fatal("store reached for invalid body")
	}
	if w := call(path, valid, "Bearer valid", "bad"); w.Code != 400 {
		t.Fatalf("invalid key: %d", w.Code)
	}
	if w := call("/homes/bad/reports/previews/"+preview+"/deliveries", valid, "Bearer valid", key); w.Code != 404 {
		t.Fatalf("invalid home: %d", w.Code)
	}
	if w := call(path, valid, "Bearer valid", key); w.Code != 201 || !store.queued || store.owner != "verified-owner" || store.delivery.PreviewID != preview || store.delivery.IdempotencyKey.String() != key || !store.delivery.Approved || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("enqueue: %d %+v %s", w.Code, store, w.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrNotFound, 404, "not_found"}, {ErrInvalidRecipients, 400, "invalid_request"},
		{ErrApprovalRequired, 400, "invalid_request"}, {ErrSnapshotMismatch, 409, "snapshot_mismatch"},
		{ErrStalePreview, 409, "preview_stale"}, {ErrKeyReuse, 409, "idempotency_key_reuse"},
		{ErrDeliveryRate, 429, "delivery_rate_limited"}, {errors.New("db unavailable"), 500, "internal_error"},
	} {
		store.err = tc.err
		w := call(path, valid, "Bearer valid", key)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}

func TestPreviewRoutesRequireOwnerAuthAndStrictSelection(t *testing.T) {
	home, appliance, preview := uuid.NewString(), uuid.NewString(), uuid.NewString()
	store := &routeStore{}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	r := chi.NewRouter()
	Service{Store: store, Auth: auth, UserID: func(*http.Request) string { return "verified-owner" }}.Routes(r)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	post := "/homes/" + home + "/reports/preview"
	noKey := httptest.NewRequest(http.MethodPost, post, strings.NewReader(`{"appliance_ids":["`+appliance+`"]}`))
	noKey.Header.Set("Content-Type", "application/json")
	noKey.Header.Set("Authorization", "Bearer valid")
	noKeyResponse := httptest.NewRecorder()
	r.ServeHTTP(noKeyResponse, noKey)
	if noKeyResponse.Code != 400 {
		t.Fatalf("missing idempotency key: %d", noKeyResponse.Code)
	}
	if w := call(http.MethodPost, post, `{"appliance_ids":["`+appliance+`"]}`, ""); w.Code != 401 || store.created {
		t.Fatalf("unauthenticated create: %d", w.Code)
	}
	for _, body := range []string{
		`{}`, `{"appliance_ids":[]}`, `{"appliance_ids":["bad"]}`,
		`{"appliance_ids":["` + appliance + `","` + appliance + `"]}`,
		`{"appliance_ids":["` + appliance + `"],"recipient":"attacker@example.com"}`,
		`{"appliance_ids":["` + appliance + `"]} {}`,
		strings.Repeat(" ", 4100) + `{"appliance_ids":["` + appliance + `"]}`,
	} {
		if w := call(http.MethodPost, post, body, "Bearer valid"); w.Code != 400 && w.Code != 413 {
			t.Fatalf("invalid body: %d %s", w.Code, body)
		}
	}
	if store.created {
		t.Fatal("store reached for invalid request")
	}
	if w := call(http.MethodPost, "/homes/bad/reports/preview", `{"appliance_ids":["`+appliance+`"]}`, "Bearer valid"); w.Code != 404 {
		t.Fatalf("invalid home: %d", w.Code)
	}
	w := call(http.MethodPost, post, `{"appliance_ids":["`+appliance+`"]}`, "Bearer valid")
	if w.Code != 201 || !store.created || store.owner != "verified-owner" || len(store.selected) != 1 || store.selected[0].String() != appliance || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("create: %d %+v %s", w.Code, store, w.Body.String())
	}
	get := "/homes/" + home + "/reports/previews/" + preview
	store.read = false
	if w := call(http.MethodGet, get, "", ""); w.Code != 401 || store.read {
		t.Fatalf("unauthenticated read: %d", w.Code)
	}
	if w := call(http.MethodGet, get, "", "Bearer valid"); w.Code != 200 || !store.read || store.owner != "verified-owner" {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	store.err = ErrNotFound
	if w := call(http.MethodGet, get, "", "Bearer valid"); w.Code != 404 {
		t.Fatalf("cross-owner read: %d", w.Code)
	}
	store.err = ErrKeyReuse
	if w := call(http.MethodPost, post, `{"appliance_ids":["`+appliance+`"]}`, "Bearer valid"); w.Code != 409 {
		t.Fatalf("key reuse: %d", w.Code)
	}
	store.err = errors.New("db unavailable")
	if w := call(http.MethodGet, get, "", "Bearer valid"); w.Code != 500 {
		t.Fatalf("internal read: %d", w.Code)
	}
}
