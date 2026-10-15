package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckLocalAuthKeysCommandDoesNotContactDatabase(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	privatePath := write("private.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	publicPath := write("public.pem", publicPEM, 0644)
	pinnedPath := write("pinned-public.pem", publicPEM, 0644)
	publicMap, err := json.Marshal(map[string]string{"current": publicPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENERGYLENS_CLOUD_ENV", "test")
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "postgres://user:pass@127.0.0.1:1/unreachable?sslmode=disable")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_ISSUER", "")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE", "")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED", "true")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED", "true")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY", "test-key")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM", "auth@example.test")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN", "https://app.example.test")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ACTIVE_KEY_ID", "current")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PRIVATE_KEY_FILE", privatePath)
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PUBLIC_KEY_FILES", string(publicMap))
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_AUDIENCE", "energylens-cloud")
	for _, name := range []string{"REGISTER", "LOGIN", "RECOVERY"} {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		path := write(name+".key", key, 0600)
		t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_"+name+"_RATE_KEY_FILE", path)
	}
	if err := run(context.Background(), []string{"check-local-auth-keys", "--retain-key-id", "current", "--retain-public-key-file", pinnedPath}); err != nil {
		t.Fatalf("local-only key check failed without database: %v", err)
	}
	t.Setenv("ENERGYLENS_CLOUD_OIDC_ISSUER", "https://id.example/")
	if err := run(context.Background(), []string{"check-local-auth-keys"}); err == nil {
		t.Fatal("preflight accepted incomplete OIDC configuration")
	}
	t.Setenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE", "external")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL", "https://id.example/jwks")
	if err := run(context.Background(), []string{"check-local-auth-keys"}); err != nil {
		t.Fatalf("dual-mode key check failed without database: %v", err)
	}
	if err := run(context.Background(), []string{"check-local-auth-keys", "--retain-key-id", "missing", "--retain-public-key-file", pinnedPath}); err == nil {
		t.Fatal("command accepted missing retained key")
	}
	t.Setenv("RUNTIME_ENV", "local")
	t.Setenv("ENERGYLENS_CLOUD_ENV", "dev")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_ISSUER", "")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE", "")
	t.Setenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RECOVERY_RATE_KEY_FILE", "")
	t.Setenv("ENERGYLENS_CLOUD_RUNTIME_LOCAL_AUTH_AUDIENCE", "energylens-local-cli-test")
	t.Setenv("ENERGYLENS_CLOUD_TLS_CERT", "/private/server.crt")
	t.Setenv("ENERGYLENS_CLOUD_TLS_KEY", "/private/server.key")
	t.Setenv("ENERGYLENS_CLOUD_HUB_CLIENT_CA", "/private/hub-ca.crt")
	if err := run(context.Background(), []string{"check-local-auth-keys"}); err != nil {
		t.Fatalf("username-local preflight required recovery/mail or contacted database: %v", err)
	}
	t.Setenv("ENERGYLENS_CLOUD_TLS_CERT", "")
	t.Setenv("ENERGYLENS_CLOUD_TLS_KEY", "")
	t.Setenv("ENERGYLENS_CLOUD_HUB_CLIENT_CA", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ACTIVE_KEY_ID", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PRIVATE_KEY_FILE", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PUBLIC_KEY_FILES", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_REGISTER_RATE_KEY_FILE", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LOGIN_RATE_KEY_FILE", "")
	if err := run(context.Background(), []string{"check-config", "worker"}); err != nil {
		t.Fatalf("local worker required API signing or TLS material: %v", err)
	}
	t.Setenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL", "postgres://migrator:pass@127.0.0.1:1/unreachable?sslmode=disable")
	if err := run(context.Background(), []string{"check-config", "migrate"}); err != nil {
		t.Fatalf("local migration configuration rejected: %v", err)
	}
}

func TestLocalMigrationEndpointBoundary(t *testing.T) {
	t.Setenv("RUNTIME_ENV", "local")
	t.Setenv("ENERGYLENS_CLOUD_ENV", "")
	for _, dsn := range []string{
		"postgres://migrator:never-print-this@db.rds.amazonaws.com/energylens?sslmode=verify-full",
		"host=127.0.0.1,db.example.com user=migrator password=never-print-this dbname=energylens sslmode=disable",
		"postgres:///energylens?sslmode=disable",
		"postgres://never-print-this:%zz@localhost/db",
	} {
		t.Setenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL", dsn)
		for _, args := range [][]string{{"check-config", "migrate"}, {"migrate"}, {"provision-hub"}, {"import-reviewed-catalog"}} {
			err := run(context.Background(), args)
			if err == nil || strings.Contains(err.Error(), "never-print-this") || strings.Contains(err.Error(), "connect migration database") {
				t.Fatalf("migration boundary did not reject safely before connecting: %v", err)
			}
		}
	}
	for _, dsn := range []string{
		"postgres://migrator:pass@127.0.0.1:1/unreachable?sslmode=disable",
		"postgres:///unreachable?host=/tmp/energylens-unreachable&sslmode=disable",
	} {
		t.Setenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL", dsn)
		if err := run(context.Background(), []string{"check-config", "migrate"}); err != nil {
			t.Fatalf("offline local migration preflight rejected explicit local endpoint: %v", err)
		}
	}
}
