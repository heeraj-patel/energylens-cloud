package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This exercises the complete local API and PostgreSQL stores through HTTPS.
// The TLS server is in-process; a guarded disposable PostgreSQL restart is optional.
func TestLocalOnboardingTwoAccountsDurableHTTP(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_onboarding_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close(ctx)
		}
		if admin != nil {
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Errorf("drop test schema: %v", err)
			}
			_ = admin.Close(ctx)
		}
	}()
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connCfg.RuntimeParams["search_path"] = schema
	conn, err = pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	keyDir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(keyDir, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	privPath := write("auth.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600)
	pubPath := write("auth.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0644)
	pubFiles, err := json.Marshal(map[string]string{"active": pubPath})
	if err != nil {
		t.Fatal(err)
	}
	rateKey := func(name string) string {
		t.Helper()
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		return write(name, key, 0600)
	}
	cfg := Config{Role: RoleAPI, RuntimeEnvironment: "local", Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: parsed.String(), LocalAuthEnabled: true, LocalAuthAudience: "energylens-local-onboarding-test", LocalAuthActiveKeyID: "active", LocalAuthPrivateKey: privPath, LocalAuthPublicKeys: string(pubFiles), LocalAuthRegisterKey: rateKey("register.key"), LocalAuthLoginKey: rateKey("login.key"), MeteredExportCursorKeyFile: rateKey("export.key"), HubCertFile: "/private/server.crt", HubKeyFile: "/private/server.key", HubCAFile: "/private/hub-ca.crt"}
	start := func() (*Runtime, *httptest.Server) {
		t.Helper()
		rt, err := Build(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return rt, httptest.NewTLSServer(rt.API)
	}
	rt, server := start()
	defer func() { server.Close(); rt.Close() }()
	client := server.Client()
	call := func(method, path, token string, body any, headers map[string]string, want int) (map[string]any, http.Header) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, raw)
		}
		var payload map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("%s %s JSON: %v", method, path, err)
			}
		}
		return payload, resp.Header
	}
	post := func(path, token string, body any, headers map[string]string, want int) map[string]any {
		out, _ := call("POST", path, token, body, headers, want)
		return out
	}
	get := func(path, token string, want int) (map[string]any, http.Header) {
		return call("GET", path, token, nil, nil, want)
	}
	list := func(path, token, field string) []any {
		t.Helper()
		out, _ := get(path, token, 200)
		items, ok := out[field].([]any)
		if !ok {
			t.Fatalf("%s missing %s: %v", path, field, out)
		}
		return items
	}
	register := func(username string) {
		t.Helper()
		post("/v1/auth/local/registration", "", map[string]string{"username": username, "password": "correct long password"}, nil, 201)
	}
	login := func(username string) (string, string) {
		t.Helper()
		out := post("/v1/auth/local/login", "", map[string]string{"username": username, "password": "correct long password"}, nil, 200)
		access, _ := out["access_token"].(string)
		refresh, _ := out["refresh_token"].(string)
		if access == "" || refresh == "" {
			t.Fatalf("login envelope missing tokens: %v", out)
		}
		return access, refresh
	}
	homeBody := func(name string) map[string]any {
		return map[string]any{"name": name, "timezone": "America/Chicago", "location": map[string]string{"country_code": "US", "postal_code": "60601", "precision": "postal_code", "source": "user_entered"}}
	}
	home := func(token, name, key string) string {
		t.Helper()
		out := post("/v1/homes/", token, homeBody(name), map[string]string{"Idempotency-Key": key}, 201)
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("home missing ID: %v", out)
		}
		return id
	}
	register("local_alice")
	call("GET", "/v1/homes/", "", nil, nil, 401)
	aToken, aRefresh := login("local_alice")
	me, _ := get("/v1/me", aToken, 200)
	if me["email"] != nil || me["email_verified"] != false || me["username"] != "local_alice" {
		t.Fatalf("local identity unexpectedly verified/email linked: %v", me)
	}
	if n := len(list("/v1/homes/", aToken, "homes")); n != 0 {
		t.Fatalf("signup created %d homes", n)
	}
	if pref, _ := get("/v1/me/preferences", aToken, 200); pref["history_disclosure_accepted_at"] != nil {
		t.Fatal("new account has a recorded disclosure")
	}
	aHome := home(aToken, "Alice home", uuid.NewString())
	applianceBody := map[string]any{"name": "Study lamp", "category": "light", "identity": map[string]string{"brand": "Fixture", "full_model": "Study Lamp 1", "fuel": "electricity", "match_quality": "unresolved"}}
	appliance := post("/v1/homes/"+aHome+"/appliances/", aToken, applianceBody, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	applianceID, _ := appliance["id"].(string)
	if applianceID == "" {
		t.Fatalf("appliance missing ID: %v", appliance)
	}
	post("/v1/auth/local/logout", "", map[string]string{"refresh_token": aRefresh}, nil, 204)
	get("/v1/me", aToken, 401)
	get("/v1/homes/"+aHome, aToken, 401)
	register("local_bob")
	bToken, _ := login("local_bob")
	if n := len(list("/v1/homes/", bToken, "homes")); n != 0 {
		t.Fatalf("Bob inherited %d homes", n)
	}
	get("/v1/homes/"+aHome, bToken, 404)
	bHome := home(bToken, "Bob home", uuid.NewString())
	if n := len(list("/v1/homes/"+bHome+"/appliances/", bToken, "appliances")); n != 0 {
		t.Fatalf("Bob home inherited %d appliances", n)
	}
	otherHome := "/v1/homes/" + aHome
	for _, path := range []string{otherHome, otherHome + "/appliances/", otherHome + "/appliances/" + applianceID, otherHome + "/sensors/", otherHome + "/attachments/", otherHome + "/metered-history"} {
		get(path, bToken, 404)
	}
	call("PATCH", otherHome, bToken, map[string]string{"name": "stolen"}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": "\"1\""}, 404)
	call("PATCH", otherHome+"/appliances/"+applianceID, bToken, map[string]string{"name": "stolen"}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": "\"1\""}, 404)
	if aHome == bHome {
		t.Fatal("two accounts shared a home ID")
	}
	aToken, _ = login("local_alice")
	if n := len(list("/v1/homes/", aToken, "homes")); n != 1 {
		t.Fatalf("Alice recovered %d homes", n)
	}
	get(otherHome, aToken, 200)
	get(otherHome+"/appliances/"+applianceID, aToken, 200)
	stableKey := uuid.NewString()
	aSecond := home(aToken, "Alice second home", stableKey)
	if n := len(list("/v1/homes/"+aSecond+"/appliances/", aToken, "appliances")); n != 0 {
		t.Fatalf("new home inherited %d appliances", n)
	}
	// Simulate a lost response and an API process restart with the same DB and keys.
	server.Close()
	rt.Close()
	if restartDir := os.Getenv("ENERGYLENS_CLOUD_TEST_POSTGRES_DATA_DIR"); restartDir != "" {
		var actualDir string
		if err := conn.QueryRow(ctx, `SHOW data_directory`).Scan(&actualDir); err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(restartDir)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := filepath.EvalSymlinks(actualDir)
		if err != nil {
			t.Fatal(err)
		}
		if want != actual || !filepath.IsAbs(want) {
			t.Fatal("refusing to restart unexpected PostgreSQL data directory")
		}
		conn.Close(ctx)
		admin.Close(ctx)
		restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(restartCtx, "pg_ctl", "-D", restartDir,
			"-l", filepath.Join(restartDir, "local-onboarding-test-restart.log"), "-m", "fast", "-w", "restart")
		cmd.WaitDelay = 5 * time.Second
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("disposable PostgreSQL restart failed: %v: %s", err, strings.TrimSpace(string(output)))
		}
		t.Log("guarded disposable PostgreSQL restart completed")
		admin, err = pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		conn, err = pgx.ConnectConfig(ctx, connCfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	rt, server = start()
	client = server.Client()
	replay := home(aToken, "Alice second home", stableKey)
	if replay != aSecond {
		t.Fatalf("stable key created a second home: %s vs %s", replay, aSecond)
	}
	if n := len(list("/v1/homes/", aToken, "homes")); n != 2 {
		t.Fatalf("API restart yielded %d Alice homes", n)
	}
	get(otherHome+"/appliances/"+applianceID, aToken, 200)
	get("/v1/homes/"+bHome, aToken, 404)
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM homes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("persisted home count %d, want 3", count)
	}
}
