package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func localRuntimeTestConfig(role Role) Config {
	return Config{
		Role: role, RuntimeEnvironment: "local", Environment: "dev", APIListen: "127.0.0.1:0",
		DatabaseURL:      "postgres://user:pass@127.0.0.1:5432/energylens?sslmode=disable",
		LocalAuthEnabled: true, LocalAuthAudience: "energylens-local-acceptance",
		LocalAuthActiveKeyID: "active", LocalAuthPrivateKey: "/private/auth.pem",
		LocalAuthPublicKeys:  `{"active":"/private/auth.pub"}`,
		LocalAuthRegisterKey: "/private/register.key", LocalAuthLoginKey: "/private/login.key",
		HubCertFile: "/private/server.crt", HubKeyFile: "/private/server.key", HubCAFile: "/private/hub-ca.crt",
	}
}

func TestLocalRuntimeConfigMatrix(t *testing.T) {
	base := localRuntimeTestConfig(RoleAPI)
	if err := base.Validate(); err != nil {
		t.Fatalf("valid local API configuration: %v", err)
	}
	worker := localRuntimeTestConfig(RoleWorker)
	worker.LocalAuthActiveKeyID, worker.LocalAuthPrivateKey, worker.LocalAuthPublicKeys = "", "", ""
	worker.LocalAuthAudience, worker.LocalAuthRegisterKey, worker.LocalAuthLoginKey = "", "", ""
	worker.HubCertFile, worker.HubKeyFile, worker.HubCAFile = "", "", ""
	if err := worker.Validate(); err != nil {
		t.Fatalf("local worker required API material: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing database":       func(c *Config) { c.DatabaseURL = "" },
		"implicit database host": func(c *Config) { c.DatabaseURL = "postgres:///energylens?sslmode=disable" },
		"public database":        func(c *Config) { c.DatabaseURL = "postgres://user:pass@db.example.com/energylens" },
		"public fallback": func(c *Config) {
			c.DatabaseURL = "host=127.0.0.1,db.example.com user=app dbname=energylens sslmode=disable"
		},
		"hosted RDS database":     func(c *Config) { c.DatabaseURL = "postgres://user:pass@db.rds.amazonaws.com/energylens" },
		"malformed database":      func(c *Config) { c.DatabaseURL = "postgres://%bad" },
		"missing TLS certificate": func(c *Config) { c.HubCertFile = "" },
		"missing Hub CA":          func(c *Config) { c.HubCAFile = "" },
		"OIDC provider":           func(c *Config) { c.OIDCIssuer = "https://id.example" },
		"mail profile":            func(c *Config) { c.LocalAuthMailEnabled = true },
		"dev auth":                func(c *Config) { c.DevAuthEnabled = true },
		"missing local audience":  func(c *Config) { c.LocalAuthAudience = "" },
		"missing login rate key":  func(c *Config) { c.LocalAuthLoginKey = "" },
		"conflicting environment": func(c *Config) { c.Environment = "production" },
		"reused email audience":   func(c *Config) { c.LocalAuthEmailAudience = c.LocalAuthAudience },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid local runtime configuration accepted")
			}
		})
	}
}

func TestRuntimeSelectionFromEnvironment(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"explicit blank":       {"RUNTIME_ENV": " "},
		"unknown":              {"RUNTIME_ENV": "unknown"},
		"conflicting legacy":   {"RUNTIME_ENV": "local", "ENERGYLENS_CLOUD_ENV": "production"},
		"explicit disabled":    {"RUNTIME_ENV": "local", "ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED": "0"},
		"conflicting nonlocal": {"RUNTIME_ENV": "test", "ENERGYLENS_CLOUD_ENV": "dev"},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range values {
				t.Setenv(key, value)
			}
			if _, err := LoadConfig("worker"); err == nil {
				t.Fatal("conflicting runtime selection accepted")
			}
		})
	}
	for _, runtime := range []string{"dev", "test", "staging", "production"} {
		t.Run(runtime, func(t *testing.T) {
			t.Setenv("RUNTIME_ENV", runtime)
			t.Setenv("ENERGYLENS_CLOUD_ENV", "")
			// Runtime mapping is exercised before later role-specific validation.
			c, err := LoadConfig("worker")
			if runtime == "dev" || runtime == "test" {
				if err != nil || c.Environment != runtime {
					t.Fatalf("runtime mapping failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
				t.Fatalf("runtime mapping did not reach database validation: %v", err)
			}
		})
	}
}

func TestMigrationRuntimeSelection(t *testing.T) {
	for name, setup := range map[string]struct {
		runtime, legacy, want string
		valid                 bool
	}{
		"local":          {"local", "", "dev", true},
		"local with dev": {"local", "dev", "dev", true},
		"explicit test":  {"test", "", "test", true},
		"local conflict": {"local", "production", "", false},
		"blank":          {" ", "", "", false},
		"unknown":        {"unknown", "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RUNTIME_ENV", setup.runtime)
			t.Setenv("ENERGYLENS_CLOUD_ENV", setup.legacy)
			got, err := MigrationEnvironment()
			if (err == nil) != setup.valid || setup.valid && got != setup.want {
				t.Fatalf("migration runtime %q, %v", got, err)
			}
		})
	}
}

func TestAuthCapabilitiesAllowlist(t *testing.T) {
	for name, setup := range map[string]struct {
		config  Config
		methods string
	}{
		"local": {localRuntimeTestConfig(RoleAPI), "username_password"},
		"email": {Config{LocalAuthEnabled: true}, "email_password"},
		"oidc":  {Config{OIDCIssuer: "https://id.example"}, "oidc"},
	} {
		t.Run(name, func(t *testing.T) {
			r := chi.NewRouter()
			setup.config.authCapabilitiesRoutes(r)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("capability route status/cache: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 6 || got["schema_version"] != float64(1) || got["methods"].([]any)[0] != setup.methods {
				t.Fatalf("unexpected capability DTO: %v", got)
			}
			if name == "local" && (got["email_verification_required"] != false || got["password_recovery_enabled"] != false || got["session_logout_revokes_access"] != true) {
				t.Fatalf("local capability flags: %v", got)
			}
		})
	}
}
