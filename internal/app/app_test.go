package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
)

func TestRoleAndDatabasePolicy(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "production", APIListen: ":8080", OIDCIssuer: "https://id.example/", OIDCAudience: "energylens", OIDCJWKSURL: "https://id.example/jwks", HubSignCertFile: "signer.crt", HubSignKeyFile: "signer.key", HubListen: ":8081", HubCertFile: "server.crt", HubKeyFile: "server.key", HubCAFile: "client-ca.crt"}
	if err := base.Validate(); err == nil {
		t.Fatal("production must require database")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	if err := base.Validate(); err == nil {
		t.Fatal("plaintext database accepted")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=require"
	if err := base.Validate(); err == nil {
		t.Fatal("unverified database TLS accepted")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=verify-full"
	if err := base.Validate(); err != nil {
		t.Fatalf("verified database rejected: %v", err)
	}
	noOIDC := base
	noOIDC.OIDCIssuer, noOIDC.OIDCAudience, noOIDC.OIDCJWKSURL = "", "", ""
	if err := noOIDC.Validate(); err == nil {
		t.Fatal("production API accepted missing OIDC settings")
	}
	partialOIDC := base
	partialOIDC.OIDCJWKSURL = ""
	if err := partialOIDC.Validate(); err == nil {
		t.Fatal("production API accepted partial OIDC settings")
	}
	base.Role = RoleAll
	if err := base.Validate(); err == nil {
		t.Fatal("production all role accepted")
	}
}

func TestListenerRouteIsolation(t *testing.T) {
	rt, err := Build(context.Background(), Config{Role: RoleAll, Environment: "dev", APIListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for _, tc := range []struct {
		handler http.Handler
		path    string
		status  int
	}{
		{rt.API, "/healthz", 200}, {rt.API, "/readyz", 200},
		{rt.API, "/v1/energy/rates?country=US&postal_code=78732", 200},
		{rt.API, "/v1/me", 404}, {rt.API, "/v1/hubs/a/sync/batches", 404},
		{rt.Hub, "/healthz", 200}, {rt.Hub, "/readyz", 200},
		{rt.Hub, "/v1/energy/rates?country=US&postal_code=78732", 404},
		{rt.Hub, "/v1/hubs/a/sync/batches", 404},
	} {
		rec := httptest.NewRecorder()
		tc.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s: got %d, want %d: %s", tc.path, rec.Code, tc.status, rec.Body.String())
		}
	}
	worker, err := Build(context.Background(), Config{Role: RoleWorker, Environment: "dev", APIListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	rec := httptest.NewRecorder()
	worker.API.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/energy/rates?country=US&postal_code=78732", nil))
	if rec.Code != 404 {
		t.Fatalf("worker served rate route: %d", rec.Code)
	}
}

func TestMainEdgeStripsIdentityHeaders(t *testing.T) {
	h := apiRouter([]Module{{Name: "check", Mount: func(r chi.Router) {
		r.Get("/check", func(w http.ResponseWriter, req *http.Request) {
			for _, name := range []string{"X-Amzn-Mtls-Clientcert", "X-Energylens-Identity-User", "X-Forwarded-User", "X-Authenticated-User"} {
				if req.Header.Get(name) != "" {
					t.Errorf("trusted identity header survived: %s", name)
				}
			}
			w.WriteHeader(204)
		})
	}}}, func(context.Context) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/v1/check", nil)
	for _, name := range []string{"X-Amzn-Mtls-Clientcert", "X-Energylens-Identity-User", "X-Forwarded-User", "X-Authenticated-User"} {
		req.Header.Set(name, "spoofed")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestHubTrustRoutesStayOnTheirEdges(t *testing.T) {
	hub := &hubtrust.Service{}
	api := apiRouter([]Module{{Name: "hub-enroll", Mount: hub.EnrollmentRoutes}}, func(context.Context) error { return nil })
	mtls := hubRouter(func(context.Context) error { return nil }, hub)
	id := "da17c425-d948-4d6b-8cc0-acf84da38c39"
	for _, tc := range []struct {
		handler http.Handler
		method  string
		path    string
		want    int
	}{
		{api, http.MethodPost, "/v1/hubs/" + id + "/enroll", http.StatusUnsupportedMediaType},
		{mtls, http.MethodPost, "/v1/hubs/" + id + "/enroll", http.StatusNotFound},
		{api, http.MethodPost, "/v1/hubs/" + id + "/certificate", http.StatusNotFound},
		{mtls, http.MethodPost, "/v1/hubs/" + id + "/certificate", http.StatusForbidden},
		{api, http.MethodGet, "/v1/hubs/" + id + "/ownership", http.StatusNotFound},
		{mtls, http.MethodGet, "/v1/hubs/" + id + "/ownership", http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Amzn-Mtls-Clientcert", "forged")
		tc.handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: got %d want %d", tc.path, rec.Code, tc.want)
		}
	}
}
