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
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalPasswordChangeRevokesCredentialsAndChallenges(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "change_password_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
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
	if err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
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
	login := LocalLoginService{DB: pool, Codec: codec, RateKey: []byte(strings.Repeat("l", 32))}
	h := LocalAuthHTTP{Login: login}
	userID := uuid.NewString()
	oldHash, err := HashLocalPassword("original long password")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,email_verified) VALUES($1,$2,true)`, userID, "person@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, userID, LocalIssuer, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash,verified_at) VALUES($1,$2,$3,$4)`, userID, "person@example.test", oldHash, now); err != nil {
		t.Fatal(err)
	}
	tokens, err := login.Login(ctx, "person@example.test", "original long password", "192.0.2.1", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := login.Login(ctx, "person@example.test", "original long password", "192.0.2.1", now)
	if err != nil {
		t.Fatal(err)
	}
	_, resetHash, err := NewLocalOpaqueToken("reset_password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_password_reset_tokens(token_hash,user_id,created_at,expires_at) VALUES($1,$2,$3,$4)`, resetHash[:], userID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	post := func(access, current, next string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"current_password": current, "new_password": next})
		req := httptest.NewRequest(http.MethodPost, "/auth/local/change-password", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if access != "" {
			req.Header.Set("Authorization", "Bearer "+access)
		}
		req.RemoteAddr = "192.0.2.1:1000"
		rec := httptest.NewRecorder()
		h.changePassword(rec, req)
		return rec.Code
	}
	if code := post("", "original long password", "new long password"); code != 401 {
		t.Fatalf("missing auth: %d", code)
	}
	if code := post(tokens.AccessToken, "wrong long password", "new long password"); code != 401 {
		t.Fatalf("wrong current: %d", code)
	}
	if code := post(tokens.AccessToken, "original long password", "short"); code != 401 {
		t.Fatalf("weak new password: %d", code)
	}
	if code := post(tokens.AccessToken, "original long password", "new long password"); code != 204 {
		t.Fatalf("change: %d", code)
	}
	if code := post(tokens.AccessToken, "new long password", "third long password"); code != 401 {
		t.Fatalf("old access token: %d", code)
	}
	if _, err := (LocalRefreshService{DB: pool, Codec: codec}).Rotate(ctx, tokens.RefreshToken, now.Add(time.Minute)); err != ErrInvalidCredential {
		t.Fatalf("first refresh survived: %v", err)
	}
	if _, err := (LocalRefreshService{DB: pool, Codec: codec}).Rotate(ctx, second.RefreshToken, now.Add(time.Minute)); err != ErrInvalidCredential {
		t.Fatalf("second refresh survived: %v", err)
	}
	var stored string
	var version, active, challenges int
	if err := pool.QueryRow(ctx, `SELECT password_hash,access_version FROM auth_password_credentials WHERE user_id=$1`, userID).Scan(&stored, &version); err != nil {
		t.Fatal(err)
	}
	if !VerifyLocalPassword(stored, "new long password") || VerifyLocalPassword(stored, "original long password") || version != 2 {
		t.Fatal("credential was not replaced and versioned")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_refresh_sessions WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_password_reset_tokens WHERE user_id=$1 AND consumed_at IS NULL`, userID).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if active != 0 || challenges != 0 {
		t.Fatalf("active sessions=%d challenges=%d", active, challenges)
	}
}
