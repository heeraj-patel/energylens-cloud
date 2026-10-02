package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestLocalAuthRuntimeGate(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_auth_app_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	keyDir := t.TempDir()
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
	write := func(name string, body []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(keyDir, name)
		if err := os.WriteFile(path, body, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	privatePath := write("private.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600)
	publicPath := write("public.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0644)
	keys := make([]string, 3)
	for i, name := range []string{"registration", "login", "recovery"} {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		keys[i] = write(name+".key", key, 0600)
	}
	publicFiles, err := json.Marshal(map[string]string{"active": publicPath})
	if err != nil {
		t.Fatal(err)
	}
	c := Config{
		Role: RoleAPI, Environment: "test", APIListen: "127.0.0.1:0", DatabaseURL: parsed.String(),
		OIDCIssuer: "https://id.example/", OIDCAudience: "external", OIDCJWKSURL: "https://id.example/jwks",
		LocalAuthEnabled: true, LocalAuthMailEnabled: true, LocalAuthResendAPIKey: "test-key", LocalAuthResendFrom: "auth@example.test", LocalAuthLinkOrigin: "https://app.example.test",
		LocalAuthActiveKeyID: "active", LocalAuthPrivateKey: privatePath, LocalAuthPublicKeys: string(publicFiles), LocalAuthAudience: "energylens-cloud",
		LocalAuthRegisterKey: keys[0], LocalAuthLoginKey: keys[1], LocalAuthRecoveryKey: keys[2],
	}
	if err := CheckLocalAuthKeys(c, "", ""); err != nil {
		t.Fatalf("operator key check rejected valid material: %v", err)
	}
	pinnedPath := write("pinned.pub", mustRead(t, publicPath), 0644)
	if err := CheckLocalAuthKeys(c, "active", pinnedPath); err != nil {
		t.Fatalf("pinned current key rejected: %v", err)
	}
	if err := CheckLocalAuthKeys(c, "active", publicPath); err == nil {
		t.Fatal("candidate public key used as its own rotation reference")
	}
	hardLink := filepath.Join(keyDir, "hard-linked-reference.pub")
	if err := os.Link(publicPath, hardLink); err != nil {
		t.Fatal(err)
	}
	if err := CheckLocalAuthKeys(c, "active", hardLink); err == nil {
		t.Fatal("hard-linked candidate used as its own rotation reference")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKIXPublicKey(otherPublic)
	if err != nil {
		t.Fatal(err)
	}
	otherPath := write("other.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: otherDER}), 0644)
	if err := CheckLocalAuthKeys(c, "active", otherPath); err == nil {
		t.Fatal("changed public key under previous ID accepted")
	}
	if err := CheckLocalAuthKeys(c, "retired", pinnedPath); err == nil {
		t.Fatal("missing previous key accepted")
	}
	if err := CheckLocalAuthKeys(c, "active", ""); err == nil {
		t.Fatal("unpaired previous key reference accepted")
	}
	rt, err := Build(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rt.API.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/local/login", nil))
	rt.Close()
	if rec.Code != http.StatusUnsupportedMediaType || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("enabled route missing or cacheable: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if err := os.WriteFile(keys[2], mustRead(t, keys[1]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLocalAuthKeys(c, "", ""); err == nil {
		t.Fatal("operator check accepted duplicate rate key material")
	}
	if rt, err := Build(ctx, c); err == nil {
		rt.Close()
		t.Fatal("duplicate rate key material accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
