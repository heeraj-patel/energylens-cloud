package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalAuthGateRequiresCompleteConfiguration(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED", "")
	loaded, err := LoadConfig("api")
	if err != nil || loaded.LocalAuthEnabled {
		t.Fatalf("local auth must default off: %v", err)
	}
	if _, verifier, err := buildLocalAuth(loaded, nil); err != nil || verifier != nil {
		t.Fatalf("disabled local auth constructed verifier: %v", err)
	}
	if err := CheckLocalAuthKeys(loaded, "", ""); err == nil {
		t.Fatal("operator check accepted disabled local auth")
	}
	base := Config{
		Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable",
		OIDCIssuer: "https://id.example/", OIDCAudience: "external", OIDCJWKSURL: "https://id.example/jwks",
		LocalAuthEnabled: true, LocalAuthMailEnabled: true, LocalAuthResendAPIKey: "test-key", LocalAuthResendFrom: "auth@example.test", LocalAuthLinkOrigin: "https://app.example.test",
		LocalAuthActiveKeyID: "active", LocalAuthPrivateKey: "/keys/private.pem", LocalAuthPublicKeys: `{"active":"/keys/public.pem"}`, LocalAuthAudience: "energylens-cloud",
		LocalAuthRegisterKey: "/keys/register", LocalAuthLoginKey: "/keys/login", LocalAuthRecoveryKey: "/keys/recovery",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("complete configuration rejected: %v", err)
	}
	for name, alter := range map[string]func(*Config){
		"disabled mail": func(c *Config) { c.LocalAuthMailEnabled = false },
		"missing OIDC":  func(c *Config) { c.OIDCIssuer = "" },
		"same issuer":   func(c *Config) { c.OIDCIssuer = "urn:energylens:local:v1" },
		"missing rate":  func(c *Config) { c.LocalAuthLoginKey = "" },
		"shared rate":   func(c *Config) { c.LocalAuthLoginKey = c.LocalAuthRegisterKey },
		"relative key":  func(c *Config) { c.LocalAuthRecoveryKey = "recovery" },
		"bad ring":      func(c *Config) { c.LocalAuthPublicKeys = `{"active":"relative.pem"}` },
		"worker":        func(c *Config) { c.Role = RoleWorker },
	} {
		c := base
		alter(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED", "maybe")
	if _, err := LoadConfig("api"); err == nil {
		t.Fatal("invalid local auth flag accepted")
	}
}

func TestLocalAuthDefaultRoutesStayAbsent(t *testing.T) {
	rt, err := Build(context.Background(), Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rec := httptest.NewRecorder()
	rt.API.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/local/login", nil))
	if rec.Code != 404 {
		t.Fatalf("default-off local auth route: %d", rec.Code)
	}
}
