package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRoleAndDatabasePolicy(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "production", APIListen: ":18091", OIDCIssuer: "https://id.example/", OIDCAudience: "energylens", OIDCJWKSURL: "https://id.example/jwks", HubSignCertFile: "signer.crt", HubSignKeyFile: "signer.key", HubCertFile: "server.crt", HubKeyFile: "server.key", HubCAFile: "client-ca.crt", MeteredExportCursorKeyFile: "/private/metered-export.key"}
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

func TestAccountDeletionProductionGate(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "production", APIListen: ":18091", DatabaseURL: "postgres://user:pass@localhost:5432/db?sslmode=verify-full", OIDCIssuer: "https://id.example/", OIDCAudience: "energylens", OIDCJWKSURL: "https://id.example/jwks", HubSignCertFile: "signer.crt", HubSignKeyFile: "signer.key", HubCertFile: "server.crt", HubKeyFile: "server.key", HubCAFile: "client-ca.crt", MeteredExportCursorKeyFile: "/private/metered-export.key"}
	if err := base.Validate(); err != nil {
		t.Fatalf("disabled route rejected: %v", err)
	}
	base.AccountDeletionEnabled = true
	if err := base.Validate(); err == nil {
		t.Fatal("production deletion route enabled before external journal assurance")
	}
	base.Environment = "test"
	if err := base.Validate(); err != nil {
		t.Fatalf("test deletion route rejected: %v", err)
	}
	base.DatabaseURL = ""
	if err := base.Validate(); err == nil {
		t.Fatal("deletion route enabled without database")
	}
}

func TestSingleListenerRoutes(t *testing.T) {
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

func TestHubTrustRoutesShareListenerWithEnrollment(t *testing.T) {
	hub := &hubtrust.Service{}
	api := cloudRouter([]Module{{Name: "hub-enroll", Mount: hub.EnrollmentRoutes}}, func(context.Context) error { return nil }, hub, nil)
	id := "da17c425-d948-4d6b-8cc0-acf84da38c39"
	for _, tc := range []struct {
		handler http.Handler
		method  string
		path    string
		want    int
	}{
		{api, http.MethodPost, "/v1/hubs/" + id + "/enroll", http.StatusUnsupportedMediaType},
		{api, http.MethodPost, "/v1/hubs/" + id + "/certificate", http.StatusForbidden},
		{api, http.MethodGet, "/v1/hubs/" + id + "/ownership", http.StatusForbidden},
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

func TestHubRouteRequiresVerifiedTLSCertificate(t *testing.T) {
	hit := false
	handler := requireVerifiedHubCertificate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, state := range []*tls.ConnectionState{nil, {PeerCertificates: []*x509.Certificate{{}}}, {VerifiedChains: [][]*x509.Certificate{{{}}}}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/hubs/hub/ownership", nil)
		req.TLS = state
		req.Header.Set("X-Amzn-Mtls-Clientcert", "forged")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || hit {
			t.Fatalf("unverified peer reached hub handler: status=%d hit=%v", rec.Code, hit)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/hubs/hub/ownership", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}, VerifiedChains: [][]*x509.Certificate{{{}}}}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !hit {
		t.Fatalf("verified peer denied: status=%d hit=%v", rec.Code, hit)
	}
}

func TestAllHubOperationsRequireVerifiedTLSCertificate(t *testing.T) {
	router := cloudRouter([]Module{{Name: "public", Mount: func(r chi.Router) {
		r.Get("/public", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}}}, func(context.Context) error { return nil }, &hubtrust.Service{}, &pgxpool.Pool{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/hubs/id/certificate"},
		{http.MethodGet, "/v1/hubs/id/ownership"},
		{http.MethodPost, "/v1/hubs/id/catalog/model-search"},
		{http.MethodGet, "/v1/hubs/id/catalog/products/product"},
		{http.MethodGet, "/v1/hubs/id/usage-facts"},
		{http.MethodGet, "/v1/hubs/id/sensors"},
		{http.MethodGet, "/v1/hubs/id/attachments"},
		{http.MethodPost, "/v1/hubs/id/sync/batches"},
		{http.MethodPost, "/v1/hubs/id/kasa-runtime/snapshots"},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: got %d, want certificate gate", tc.method, tc.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/public", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("public route required a certificate: %d", rec.Code)
	}
}

func TestSinglePortConfiguration(t *testing.T) {
	c := Config{Role: RoleAPI, Environment: "dev", APIListen: ":18091", HubCertFile: "server.crt", HubKeyFile: "server.key", HubCAFile: "client-ca.crt"}
	if err := c.Validate(); err != nil {
		t.Fatalf("single TLS listener rejected: %v", err)
	}
	c.HubListen = ":18092"
	if err := c.Validate(); err == nil {
		t.Fatal("second listener accepted")
	}
	c.HubListen = ""
	c.Environment = "production"
	c.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=verify-full"
	c.OIDCIssuer, c.OIDCAudience, c.OIDCJWKSURL = "https://id.example/", "app", "https://id.example/jwks"
	c.HubSignCertFile, c.HubSignKeyFile = "signer.crt", "signer.key"
	c.MeteredExportCursorKeyFile = "/keys/metered-export.key"
	c.HubCertFile = ""
	if err := c.Validate(); err == nil {
		t.Fatal("production API without TLS certificate accepted")
	}
}

func TestAPIServeRequiresTLSMaterial(t *testing.T) {
	err := Serve(context.Background(), Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("API server started without TLS material")
	}
}

func TestAPIServePreflightsTLSBeforeDatabase(t *testing.T) {
	err := Serve(context.Background(), Config{
		Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0",
		DatabaseURL: "postgres://user:pass@127.0.0.1:1/db?sslmode=disable",
		HubCertFile: "/missing/cloud.crt", HubKeyFile: "/missing/cloud.key", HubCAFile: "/missing/hub-ca.crt",
	})
	if err == nil || !strings.Contains(err.Error(), "load hub server certificate") {
		t.Fatalf("TLS material was not checked first: %v", err)
	}
}

func TestWorkerServeDoesNotOpenListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Serve(ctx, Config{Role: RoleWorker, Environment: "dev", APIListen: "invalid-listen-address"})
	if err != nil {
		t.Fatalf("worker required listener: %v", err)
	}
}
