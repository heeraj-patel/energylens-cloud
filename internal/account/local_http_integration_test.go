package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type localHTTPFakeSender struct{ verification, recovery []string }

func (s *localHTTPFakeSender) SendVerification(_ context.Context, _, token string) error {
	s.verification = append(s.verification, token)
	return nil
}
func (s *localHTTPFakeSender) SendRecovery(_ context.Context, _, token string) error {
	s.recovery = append(s.recovery, token)
	return nil
}

func TestLocalHTTPInactiveAndFullPrivateRoundTrip(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_http_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}
	sender := &localHTTPFakeSender{}
	h := LocalAuthHTTP{Registration: LocalRegistrationService{DB: pool, Sender: sender, RateKey: []byte(strings.Repeat("r", 32))}, Login: LocalLoginService{DB: pool, Codec: codec, RateKey: []byte(strings.Repeat("l", 32))}, Refresh: LocalRefreshService{DB: pool, Codec: codec}, Recovery: LocalRecoveryService{DB: pool, Sender: sender, RateKey: []byte(strings.Repeat("c", 32))}}
	router := func(service LocalAuthHTTP) http.Handler {
		r := chi.NewRouter()
		r.Route("/v1", service.Routes)
		return r
	}
	request := func(api http.Handler, path string, body any, peer string) *httptest.ResponseRecorder {
		t.Helper()
		data, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100.45")
		req.RemoteAddr = peer
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}
	path := "/v1/auth/local/registration"
	if rec := request(router(h), path, map[string]string{"email": "person@example.test", "password": "original long password"}, "192.0.2.1:1000"); rec.Code != 404 {
		t.Fatalf("default-off route: %d", rec.Code)
	}
	h.Enabled = true
	incomplete := h
	incomplete.Recovery = LocalRecoveryService{}
	if rec := request(router(incomplete), path, map[string]string{"email": "person@example.test", "password": "original long password"}, "192.0.2.1:1000"); rec.Code != 404 {
		t.Fatalf("partial service route: %d", rec.Code)
	}
	api := router(h)
	post := func(path string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := request(api, path, body, "192.0.2.1:1000")
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", path, rec.Code, want, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("auth response cache/referrer headers absent")
		}
		return rec
	}
	post(path, map[string]string{"email": "person@example.test", "password": "original long password"}, 202)
	if len(sender.verification) != 1 {
		t.Fatal("registration did not deliver fake challenge")
	}
	// A spoofed forwarding header must never become the private rate key.
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_registration_rate_limits WHERE scope='source_hour' AND key_hash=$1`, h.Registration.rateHash("source", "192.0.2.1")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("socket peer not used for rate key: %d %v", count, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_registration_rate_limits WHERE scope='source_hour' AND key_hash=$1`, h.Registration.rateHash("source", "198.51.100.45")).Scan(&count); err != nil || count != 0 {
		t.Fatal("spoofed forwarding header used")
	}
	proxyPolicy, policyErr := ParseLocalAuthSourcePolicy("10.20.0.0/16")
	if policyErr != nil {
		t.Fatal(policyErr)
	}
	proxied := h
	proxied.SourcePolicy = proxyPolicy
	proxiedAPI := router(proxied)
	if rec := request(proxiedAPI, path, map[string]string{"email": "proxy@example.test", "password": "original long password"}, "10.20.1.2:1000"); rec.Code != 202 {
		t.Fatalf("trusted proxy single-hop request: %d", rec.Code)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_registration_rate_limits WHERE scope='source_hour' AND key_hash=$1`, h.Registration.rateHash("source", "198.51.100.45")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("trusted proxy client rate key missing: %d %v", count, err)
	}
	chainReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"spoof@example.test","password":"original long password"}`))
	chainReq.Header.Set("Content-Type", "application/json")
	chainReq.Header.Set("X-Forwarded-For", "203.0.113.5, 198.51.100.45")
	chainReq.RemoteAddr = "10.20.1.2:1000"
	chainRec := httptest.NewRecorder()
	proxiedAPI.ServeHTTP(chainRec, chainReq)
	if chainRec.Code != 401 {
		t.Fatalf("prepopulated proxy chain accepted: %d", chainRec.Code)
	}
	post("/v1/auth/local/verify-email", map[string]string{"token": sender.verification[0]}, 204)
	login := post("/v1/auth/local/login", map[string]string{"email": "person@example.test", "password": "original long password"}, 200)
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if err = json.Unmarshal(login.Body.Bytes(), &tokens); err != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.TokenType != "Bearer" {
		t.Fatalf("invalid login tokens: %v", err)
	}
	rotated := post("/v1/auth/local/refresh", map[string]string{"refresh_token": tokens.RefreshToken}, 200)
	var next struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err = json.Unmarshal(rotated.Body.Bytes(), &next); err != nil || next.RefreshToken == tokens.RefreshToken || next.RefreshToken == "" {
		t.Fatalf("refresh did not rotate: %v", err)
	}
	post("/v1/auth/local/logout", map[string]string{"refresh_token": next.RefreshToken}, 204)
	post("/v1/auth/local/refresh", map[string]string{"refresh_token": next.RefreshToken}, 401)
	post("/v1/auth/local/recovery", map[string]string{"email": "person@example.test"}, 202)
	if len(sender.recovery) != 1 {
		t.Fatal("recovery did not deliver fake challenge")
	}
	post("/v1/auth/local/reset-password", map[string]string{"token": sender.recovery[0], "password": "replacement long password"}, 204)
	post("/v1/auth/local/login", map[string]string{"email": "person@example.test", "password": "original long password"}, 401)
	post("/v1/auth/local/login", map[string]string{"email": "person@example.test", "password": "replacement long password"}, 200)
	post("/v1/auth/local/recovery", map[string]string{"email": "unknown@example.test"}, 202)
	if len(sender.recovery) != 1 {
		t.Fatal("unknown recovery sent email")
	}
	bad := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"person@example.test","password":"replacement long password","extra":1}`))
	bad.Header.Set("Content-Type", "application/json")
	bad.RemoteAddr = "192.0.2.1:1000"
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, bad)
	if rec.Code != 400 || strings.Contains(rec.Body.String(), "person@example.test") {
		t.Fatalf("unknown field leaked or accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec = request(api, path, map[string]string{"email": "other@example.test", "password": "replacement long password"}, "not-a-peer"); rec.Code != 401 {
		t.Fatalf("untrusted source accepted: %d", rec.Code)
	}
}
