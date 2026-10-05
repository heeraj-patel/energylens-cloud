package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestDevAuthRequiresDevDatabaseAndSeparateAuth(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://localhost/dev", DevAuthEnabled: true,
		DevAuthEmail: "owner@example.test", DevAuthPassword: "test1EneryLens"}
	if err := base.Validate(); err != nil {
		t.Fatalf("dev config: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"staging":       func(c *Config) { c.Environment = "staging" },
		"production":    func(c *Config) { c.Environment = "production" },
		"worker":        func(c *Config) { c.Role = RoleWorker },
		"no database":   func(c *Config) { c.DatabaseURL = "" },
		"weak password": func(c *Config) { c.DevAuthPassword = strings.Repeat("p", 11) },
		"oidc":          func(c *Config) { c.OIDCIssuer = "https://id.example" },
		"local":         func(c *Config) { c.LocalAuthEnabled = true },
		"flag off":      func(c *Config) { c.DevAuthEnabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			change(&candidate)
			if candidate.Validate() == nil {
				t.Fatal("unsafe dev auth config accepted")
			}
		})
	}
}

func TestDevAuthAcceptsTwelveCharacterPassword(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://localhost/dev", DevAuthEnabled: true,
		DevAuthEmail: "owner@example.test", DevAuthPassword: strings.Repeat("p", 12)}
	if err := base.Validate(); err != nil {
		t.Fatalf("12-character password rejected: %v", err)
	}
	base.DevAuthPassword = strings.Repeat("p", 11)
	if err := base.Validate(); err == nil {
		t.Fatal("11-character password accepted")
	}
}

func TestDevAuthThrottlesFailedLogins(t *testing.T) {
	const password = "test1EneryLens"
	d := newDevAuth(Config{DevAuthEmail: "owner@example.test", DevAuthPassword: password})
	router := chi.NewRouter()
	router.Route("/v1", d.Routes)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/local/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < devAuthMaxLoginFailures; i++ {
		if got := post(`{"email":"owner@example.test","password":"wrong"}`).Code; got != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want 401", i+1, got)
		}
	}
	if got := post(`{"email":"owner@example.test","password":"` + password + `"}`).Code; got != http.StatusTooManyRequests {
		t.Fatalf("login after failure limit status = %d, want 429", got)
	}
	d.mu.Lock()
	d.failureWindowStart = time.Now().Add(-devAuthFailureWindow)
	d.mu.Unlock()
	if got := post(`{"email":"owner@example.test","password":"` + password + `"}`).Code; got != http.StatusOK {
		t.Fatalf("login after window status = %d, want 200", got)
	}
}

func TestDevAuthLoginRefreshLogoutAndOwnerIdentity(t *testing.T) {
	d := newDevAuth(Config{DevAuthEmail: "owner@example.test", DevAuthPassword: strings.Repeat("p", 40)})
	router := chi.NewRouter()
	router.Route("/v1", d.Routes)
	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	wrong := post("/v1/auth/local/login", `{"email":"owner@example.test","password":"wrong"}`)
	if wrong.Code != 401 {
		t.Fatalf("wrong password: %d", wrong.Code)
	}
	login := post("/v1/auth/local/login", `{"email":"owner@example.test","password":"`+strings.Repeat("p", 40)+`"}`)
	if login.Code != 200 || login.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login: %d", login.Code)
	}
	var issued struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &issued); err != nil || issued.Access == "" || issued.Refresh == "" {
		t.Fatalf("tokens: %v", err)
	}
	identity, err := d.Verify(context.Background(), issued.Access)
	if err != nil || identity.Issuer != devAuthIssuer || identity.Email != "owner@example.test" || !identity.EmailVerified {
		t.Fatalf("owner identity: %+v %v", identity, err)
	}
	refresh := post("/v1/auth/local/refresh", `{"refresh_token":"`+issued.Refresh+`"}`)
	if refresh.Code != 200 {
		t.Fatalf("refresh: %d", refresh.Code)
	}
	if _, err := d.Verify(context.Background(), issued.Access); err == nil {
		t.Fatal("rotated access remained valid")
	}
	if again := post("/v1/auth/local/refresh", `{"refresh_token":"`+issued.Refresh+`"}`); again.Code != 401 {
		t.Fatal("old refresh replay accepted")
	}
	if err := json.Unmarshal(refresh.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if out := post("/v1/auth/local/logout", `{"refresh_token":"`+issued.Refresh+`"}`); out.Code != 204 {
		t.Fatalf("logout: %d", out.Code)
	}
	if _, err := d.Verify(context.Background(), issued.Access); err == nil {
		t.Fatal("logged-out access remained valid")
	}
}
