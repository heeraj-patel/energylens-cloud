package designfixture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"energylens.local/cloud/internal/account"
	"github.com/go-chi/chi/v5"
)

const testUser = "11111111-1111-4111-8111-111111111111"
const otherUser = "22222222-2222-4222-8222-222222222222"
const testHome = "33333333-3333-4333-8333-333333333333"

type homes struct{}

func (homes) Get(_ context.Context, user, home string) (account.Home, error) {
	if user == testUser && home == testHome {
		return account.Home{ID: home}, nil
	}
	return account.Home{}, account.ErrNotFound
}

func service(allowed map[string]bool, user string) http.Handler {
	s := Service{Homes: homes{}, Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, UserID: func(*http.Request) string { return user }, AllowedUsers: allowed, AllowedHomes: map[string]bool{testHome: true, otherUser: true}}
	r := chi.NewRouter()
	r.Route("/v1", s.Routes)
	return r
}

func request(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer valid")
	h.ServeHTTP(w, r)
	return w
}

func TestAllFixturesExactAndInert(t *testing.T) {
	h := service(map[string]bool{testUser: true}, testUser)
	for page := range pages {
		w := request(h, "/v1/homes/"+testHome+"/design-pages/"+page)
		if w.Code != 200 {
			t.Fatalf("%s: %d", page, w.Code)
		}
		var got struct {
			HomeID          string          `json:"home_id"`
			Page            string          `json:"page"`
			DataMode        string          `json:"data_mode"`
			FixtureRevision string          `json:"fixture_revision"`
			ActionsEnabled  bool            `json:"actions_enabled"`
			Fixture         json.RawMessage `json:"fixture"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want, err := fixtures.ReadFile("fixtures/" + page + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		if err := json.Unmarshal(got.Fixture, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &b); err != nil {
			t.Fatal(err)
		}
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		if string(aj) != string(bj) {
			t.Fatalf("%s fixture changed", page)
		}
		if got.HomeID != testHome || got.Page != page || got.DataMode != "sample" || got.FixtureRevision != Revision || got.ActionsEnabled {
			t.Fatalf("%s unsafe envelope: %+v", page, got)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s cacheable", page)
		}
	}
}

func TestFixtureIsolation(t *testing.T) {
	allowed := map[string]bool{testUser: true}
	cases := []struct {
		h    http.Handler
		path string
		want int
	}{
		{service(allowed, otherUser), "/v1/homes/" + testHome + "/design-pages/dashboard", 404},
		{service(allowed, testUser), "/v1/homes/" + otherUser + "/design-pages/dashboard", 404},
		{service(allowed, testUser), "/v1/homes/" + testHome + "/design-pages/unknown", 404},
		{service(allowed, testUser), "/v1/homes/" + testHome + "/design-pages/dashboard?mode=sample", 400},
	}
	for _, tc := range cases {
		if got := request(tc.h, tc.path).Code; got != tc.want {
			t.Fatalf("%s: %d want %d", tc.path, got, tc.want)
		}
	}
	w := httptest.NewRecorder()
	service(allowed, testUser).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/homes/"+testHome+"/design-pages/dashboard", nil))
	if w.Code != 401 {
		t.Fatalf("no bearer: %d", w.Code)
	}
}

func TestParseUserIDs(t *testing.T) {
	for _, raw := range []string{"", testUser + ",", "*", "not-a-uuid"} {
		if _, err := ParseUserIDs(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if ids, err := ParseUserIDs(testUser); err != nil || !ids[testUser] {
		t.Fatalf("valid id: %v", err)
	}
}
