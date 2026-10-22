package tariff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const contractFixtures = "../../../energylens-common-contracts/api/testdata/cloud-p1-p2.json"

type planRouteStore struct {
	view               UtilityRatePlans
	err                error
	eiaid, plan, owner string
	hashes             [][]byte
}

func (s *planRouteStore) RatePlans(_ context.Context, owner, home string, _ time.Time) (UtilityRatePlans, error) {
	s.owner = owner
	return s.view, s.err
}
func (s *planRouteStore) ChooseUtility(_ context.Context, owner, home, eiaid, key string, hash []byte, _ time.Time) (UtilityRatePlans, error) {
	s.eiaid, s.hashes = eiaid, append(s.hashes, hash)
	return s.view, s.err
}
func (s *planRouteStore) ChoosePlan(_ context.Context, owner, home, plan, key string, hash []byte, _ time.Time) (UtilityRatePlans, error) {
	s.plan, s.hashes = plan, append(s.hashes, hash)
	return s.view, s.err
}

// contractPlanList is the canonical UtilityRatePlanList golden payload from the contracts repo.
func contractPlanList(t *testing.T) (UtilityRatePlans, any) {
	t.Helper()
	raw, err := os.ReadFile(contractFixtures)
	if err != nil {
		t.Fatalf("contracts sibling checkout required: %v", err)
	}
	var fixtures struct {
		Valid []struct {
			Schema string          `json:"schema"`
			Value  json.RawMessage `json:"value"`
		} `json:"valid"`
	}
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures.Valid {
		if f.Schema != "UtilityRatePlanList" {
			continue
		}
		var view UtilityRatePlans
		d := json.NewDecoder(bytes.NewReader(f.Value))
		d.DisallowUnknownFields()
		if err = d.Decode(&view); err != nil {
			t.Fatalf("contract fixture does not fit the Go response: %v", err)
		}
		var generic any
		if err = json.Unmarshal(f.Value, &generic); err != nil {
			t.Fatal(err)
		}
		return view, generic
	}
	t.Fatal("no UtilityRatePlanList fixture")
	return UtilityRatePlans{}, nil
}

func TestRatePlanRoutesMatchTheContract(t *testing.T) {
	view, golden := contractPlanList(t)
	store := &planRouteStore{view: view}
	service := Service{Store: &routeStore{}, Plans: store, Auth: func(next http.Handler) http.Handler {
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
	call := func(method, path, body, key, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/homes/"+home+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer owner")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	sameAsContract := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		var got any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, golden) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	sameAsContract("list", call("GET", "/utility-rate-plans", "", "", ""))
	if store.owner != "owner" {
		t.Fatalf("owner %q", store.owner)
	}
	key := uuid.NewString()
	sameAsContract("utility", call("PUT", "/electric-utility", `{"eiaid":"16609"}`, key, "application/json"))
	sameAsContract("plan", call("PUT", "/utility-rate-plan", `{"plan_id":"`+view.Plans[0].ID+`"}`, key, "application/json"))
	if store.eiaid != "16609" || store.plan != view.Plans[0].ID || bytes.Equal(store.hashes[0], store.hashes[1]) {
		t.Fatalf("store saw %q %q, hashes equal=%v", store.eiaid, store.plan, bytes.Equal(store.hashes[0], store.hashes[1]))
	}
	for _, tc := range []struct {
		name, path, body, key, contentType string
		status                             int
	}{
		{"no key", "/electric-utility", `{"eiaid":"16609"}`, "", "application/json", 400},
		{"bad key", "/electric-utility", `{"eiaid":"16609"}`, "x", "application/json", 400},
		{"not json", "/electric-utility", `{"eiaid":"16609"}`, key, "text/plain", 415},
		{"unknown field", "/electric-utility", `{"eiaid":"16609","x":1}`, key, "application/json", 400},
		{"two objects", "/electric-utility", `{"eiaid":"16609"}{}`, key, "application/json", 400},
		{"bad eiaid", "/electric-utility", `{"eiaid":"PGE"}`, key, "application/json", 400},
		{"missing eiaid", "/electric-utility", `{}`, key, "application/json", 400},
		{"bad plan id", "/utility-rate-plan", `{"plan_id":"x"}`, key, "application/json", 400},
	} {
		if w := call("PUT", tc.path, tc.body, tc.key, tc.contentType); w.Code != tc.status {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	for err, status := range map[error]int{ErrNotFound: 404, ErrKeyReuse: 409, ErrPlanNotOffered: 409, ErrNoBaseRate: 409, ErrUnknownUtility: 400} {
		store.err = err
		if w := call("PUT", "/utility-rate-plan", `{"plan_id":"`+view.Plans[0].ID+`"}`, key, "application/json"); w.Code != status {
			t.Errorf("%v: %d", err, w.Code)
		}
	}
	badHome := httptest.NewRequest("GET", "/v1/homes/x/utility-rate-plans", nil)
	badHome.Header.Set("Authorization", "Bearer owner")
	notFound := httptest.NewRecorder()
	if r.ServeHTTP(notFound, badHome); notFound.Code != 404 {
		t.Fatalf("bad home id %d", notFound.Code)
	}
	unauthenticated := httptest.NewRecorder()
	r.ServeHTTP(unauthenticated, httptest.NewRequest("GET", "/v1/homes/"+home+"/utility-rate-plans", nil))
	if unauthenticated.Code != 401 {
		t.Fatalf("unauthenticated list %d", unauthenticated.Code)
	}
}
