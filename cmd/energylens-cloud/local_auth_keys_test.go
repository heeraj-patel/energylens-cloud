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
}
