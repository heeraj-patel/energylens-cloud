package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func usernameTestDatabase(t *testing.T, before0073 bool) (*pgxpool.Pool, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_username_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if before0073 {
		names, err := migrate.Files(migrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		prior := fstest.MapFS{}
		for _, name := range names {
			if name >= "0073" {
				continue
			}
			body, err := migrations.FS.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			prior[name] = &fstest.MapFile{Data: body}
		}
		if err := migrate.Apply(ctx, conn, prior); err != nil {
			t.Fatal(err)
		}
	} else if err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, conn
}

func usernameServices(t *testing.T, pool *pgxpool.Pool) (LocalRegistrationService, LocalLoginService, LocalRefreshService) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-runtime-local-test", Profile: LocalUsernameProfile}
	return LocalRegistrationService{DB: pool, RateKey: []byte(strings.Repeat("r", 32)), Profile: LocalUsernameProfile},
		LocalLoginService{DB: pool, Codec: codec, RateKey: []byte(strings.Repeat("l", 32))}, LocalRefreshService{DB: pool, Codec: codec}
}

func TestUsernameMigrationPreservesEmailHomeAndRefresh(t *testing.T) {
	pool, conn := usernameTestDatabase(t, true)
	ctx := context.Background()
	id := uuid.NewString()
	home := uuid.NewString()
	hash, err := HashLocalPassword("correct long password")
	if err != nil {
		t.Fatal(err)
	}
	rawRefresh, tokenHash, err := NewLocalOpaqueToken("refresh")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,email_verified) VALUES($1,'existing@example.test',true)`, []any{id}},
		{`INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, []any{id, LocalIssuer, id}},
		{`INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash,verified_at) VALUES($1,'existing@example.test',$2,now())`, []any{id, hash}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Existing home','America/Chicago')`, []any{home, id}},
		{`INSERT INTO auth_refresh_sessions(user_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 day')`, []any{id, uuid.NewString(), tokenHash[:]}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var kind, email, owner string
	var sessions int
	if err := pool.QueryRow(ctx, `SELECT credential_kind,email_normalized FROM auth_password_credentials WHERE user_id=$1`, id).Scan(&kind, &email); err != nil {
		t.Fatal(err)
	}
	if kind != "email" || email != "existing@example.test" {
		t.Fatalf("email credential changed: %q %q", kind, email)
	}
	if err := pool.QueryRow(ctx, `SELECT owner_user_id FROM homes WHERE id=$1`, home).Scan(&owner); err != nil || owner != id {
		t.Fatalf("home changed: %s %v", owner, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_refresh_sessions WHERE user_id=$1 AND token_hash=$2`, id, tokenHash[:]).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("refresh changed: %d %v", sessions, err)
	}
	// The original email login remains usable after the additive migration.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	emailLogin := LocalLoginService{DB: pool, Codec: LocalAccessCodec{Private: private, ActiveKeyID: "email", Keys: map[string]ed25519.PublicKey{"email": public}, Audience: "energylens-cloud"}, RateKey: []byte(strings.Repeat("e", 32))}
	if _, err := emailLogin.Login(ctx, "existing@example.test", "correct long password", "192.0.2.1", time.Now()); err != nil {
		t.Fatalf("email login after migration: %v", err)
	}
	if _, err := (LocalRefreshService{DB: pool, Codec: emailLogin.Codec}).Rotate(ctx, rawRefresh, time.Now()); err != nil {
		t.Fatalf("pre-migration email refresh after migration: %v", err)
	}
	for _, q := range []string{
		`UPDATE auth_password_credentials SET email_normalized=NULL WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET activated_at=now() WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET credential_kind='username_local' WHERE user_id=$1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err == nil {
			t.Fatalf("invalid email mode accepted: %s", q)
		}
	}
}

func TestUsernameRegistrationConcurrentAndModeIsolation(t *testing.T) {
	pool, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	registration, login, refresh := usernameServices(t, pool)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, source := range []string{"192.0.2.1", "192.0.2.2"} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			_, err := registration.RegisterUsername(ctx, "  ALICE.Name  ", "correct long password", source, now)
			results <- err
		}(source)
	}
	wg.Wait()
	close(results)
	created, collisions := 0, 0
	for err := range results {
		if err == nil {
			created++
		} else if errors.Is(err, ErrUsernameInUse) {
			collisions++
		} else {
			t.Fatal(err)
		}
	}
	if created != 1 || collisions != 1 {
		t.Fatalf("concurrent outcomes: created=%d collisions=%d", created, collisions)
	}
	var id, kind, stored string
	var email *string
	var emailVerified bool
	var activated time.Time
	if err := pool.QueryRow(ctx, `SELECT c.user_id,c.credential_kind,c.password_hash,c.email_normalized,c.activated_at,u.email_verified
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id WHERE c.username_normalized='alice.name'`).Scan(&id, &kind, &stored, &email, &activated, &emailVerified); err != nil {
		t.Fatal(err)
	}
	if kind != "username_local" || stored == "correct long password" || !VerifyLocalPassword(stored, "correct long password") || email != nil || emailVerified || activated.IsZero() {
		t.Fatal("username credential activation or profile is wrong")
	}
	var users, identities int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE issuer=$1 AND subject=$2`, LocalUsernameIssuer, id).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if users != 1 || identities != 1 {
		t.Fatalf("partial/duplicate registration: users=%d identities=%d", users, identities)
	}
	tokens, err := login.Login(ctx, "ALICE.Name", "correct long password", "192.0.2.3", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalAccessVerifier{DB: pool, Codec: login.Codec}).Verify(ctx, tokens.AccessToken); err != nil {
		t.Fatalf("username token: %v", err)
	}
	if _, err := refresh.Rotate(ctx, tokens.RefreshToken, now.Add(time.Minute)); err != nil {
		t.Fatalf("username refresh: %v", err)
	}
	if _, err := login.Login(ctx, "alice.name", "wrong long password", "192.0.2.4", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("wrong password: %v", err)
	}
	emailCodec := login.Codec
	emailCodec.Profile = LocalEmailProfile
	if _, err := (LocalAccessVerifier{DB: pool, Codec: emailCodec}).Verify(ctx, tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("username token accepted in email profile: %v", err)
	}
	if _, err := (LocalLoginService{DB: pool, Codec: emailCodec, RateKey: login.RateKey}).Login(ctx, "alice.name@example.test", "correct long password", "192.0.2.5", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("username credential accepted in email profile: %v", err)
	}
	user, err := (PGStore{Pool: pool}).Resolve(ctx, Identity{Issuer: LocalUsernameIssuer, Subject: id})
	if err != nil || user.AuthMethod != "username_password" || user.Username == nil || *user.Username != "alice.name" || user.Email != nil || user.EmailVerified {
		t.Fatalf("/me identity: %+v %v", user, err)
	}
	if _, err := (PGStore{Pool: pool}).Resolve(ctx, Identity{Issuer: LocalUsernameIssuer, Subject: uuid.NewString()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown first-party identity auto-created: %v", err)
	}
	for _, q := range []string{
		`UPDATE auth_password_credentials SET email_normalized='fake@example.test' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized='BAD NAME' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET activated_at=NULL WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized='éclair' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized='ab' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized=repeat('a',65) WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized=' alice.name' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET username_normalized='alice.name ' WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET verified_at=now() WHERE user_id=$1`,
		`UPDATE auth_password_credentials SET credential_kind='email' WHERE user_id=$1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err == nil {
			t.Fatalf("invalid mode combination accepted: %s", q)
		}
	}
	if _, err := registration.RequestRegistration(ctx, "fake@example.test", "correct long password", "192.0.2.9", now); err == nil {
		t.Fatal("email registration allowed in username profile")
	}
	if err := registration.VerifyEmail(ctx, strings.Repeat("a", 43), now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("email verification allowed: %v", err)
	}
}

func TestUsernameRegistrationRateSurvivesCollisionsAndRestart(t *testing.T) {
	pool, _ := usernameTestDatabase(t, false)
	registration, _, _ := usernameServices(t, pool)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		_, err := registration.RegisterUsername(context.Background(), "Limited", "correct long password", fmt.Sprintf("192.0.2.%d", i+1), now)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i > 0 && !errors.Is(err, ErrUsernameInUse) {
			t.Fatalf("collision %d: %v", i, err)
		}
	}
	// Reconstruct the service to prove the budget lives in PostgreSQL.
	restarted := LocalRegistrationService{DB: pool, RateKey: registration.RateKey, Profile: LocalUsernameProfile}
	if _, err := restarted.RegisterUsername(context.Background(), "limited", "correct long password", "198.51.100.90", now); !errors.Is(err, ErrRegistrationRateLimited) {
		t.Fatalf("persistent throttle: %v", err)
	}
	var attempts, users int
	if err := pool.QueryRow(context.Background(), `SELECT attempts FROM auth_registration_rate_limits WHERE scope='username_day' AND key_hash=$1`, registration.rateHash("username", "limited")).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if attempts != 11 || users != 1 {
		t.Fatalf("throttle or rollback wrong: attempts=%d users=%d", attempts, users)
	}
}

func TestUsernameHTTPStrictDispatch(t *testing.T) {
	pool, _ := usernameTestDatabase(t, false)
	registration, login, refresh := usernameServices(t, pool)
	h := LocalAuthHTTP{Enabled: true, Registration: registration, Login: login, Refresh: refresh}
	r := chi.NewRouter()
	r.Route("/v1", h.Routes)
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.50:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"verify-email", "recovery", "change-email"} {
		if got := post("/v1/auth/local/"+path, map[string]string{}).Code; got != 404 {
			t.Fatalf("email route %s mounted: %d", path, got)
		}
	}
	for _, body := range []any{
		map[string]string{"email": "alice@example.test", "password": "correct long password"},
		map[string]string{"username": "alice", "password": "correct long password", "issuer": LocalUsernameIssuer},
		map[string]string{"username": "a b", "password": "correct long password"},
		map[string]string{"username": "alice", "password": "1234567"},
		map[string]string{"username": "alice", "password": "ééééééé"},
	} {
		if w := post("/v1/auth/local/registration", body); w.Code != 400 {
			t.Fatalf("invalid registration %d %s", w.Code, w.Body.String())
		}
	}
	if w := post("/v1/auth/local/registration", map[string]string{"username": "alice", "password": "12345678"}); w.Code != 201 || !strings.Contains(w.Body.String(), `"account_created"`) {
		t.Fatalf("registration %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/registration", map[string]string{"username": "unicode", "password": "éééééééé"}); w.Code != 201 {
		t.Fatalf("eight-character registration %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "unicode", "password": "éééééééé"}); w.Code != 200 {
		t.Fatalf("eight-character login %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "unicode", "password": "ééééééé"}); w.Code != 400 {
		t.Fatalf("seven-character login %d", w.Code)
	}
	if w := post("/v1/auth/local/registration", map[string]string{"username": "ALICE", "password": "12345678"}); w.Code != 409 {
		t.Fatalf("collision %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"email": "alice@example.test", "password": "correct long password"}); w.Code != 400 {
		t.Fatalf("email login accepted %d", w.Code)
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "a b", "password": "correct long password"}); w.Code != 400 {
		t.Fatalf("malformed login %d", w.Code)
	}
	unknown := post("/v1/auth/local/login", map[string]string{"username": "unknown", "password": "12345678"})
	wrong := post("/v1/auth/local/login", map[string]string{"username": "alice", "password": "87654321"})
	var unknownError, wrongError apiError
	if err := json.Unmarshal(unknown.Body.Bytes(), &unknownError); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wrong.Body.Bytes(), &wrongError); err != nil {
		t.Fatal(err)
	}
	if unknown.Code != 401 || wrong.Code != 401 || unknownError.Code != "invalid_credential" || wrongError.Code != unknownError.Code || wrongError.Message != unknownError.Message {
		t.Fatalf("incorrect credential status differs: unknown=%d wrong=%d", unknown.Code, wrong.Code)
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "alice", "password": "12345678"}); w.Code != 200 || !strings.Contains(w.Body.String(), `"access_token"`) {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "alice", "password": "1234567"}); w.Code != 400 {
		t.Fatalf("seven-character login %d", w.Code)
	}
	for _, body := range []any{
		map[string]string{"email": "alice@example.test", "password": "another long password"},
		map[string]string{"token": strings.Repeat("a", 43), "password": "another long password"},
		map[string]string{"username": "alice", "password": "another long password", "email": "alice@example.test"},
		map[string]string{"username": "alice", "password": "1234567"},
		map[string]string{"username": "alice", "password": "ééééééé"},
	} {
		if w := post("/v1/auth/local/reset-password", body); w.Code != 400 {
			t.Fatalf("invalid reset %d %s", w.Code, w.Body.String())
		}
	}
	if w := post("/v1/auth/local/reset-password", map[string]string{"username": "unknown", "password": "abcdefgh"}); w.Code != 204 {
		t.Fatalf("unknown reset %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/reset-password", map[string]string{"username": "ALICE", "password": "abcdefgh"}); w.Code != 204 {
		t.Fatalf("username reset %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/reset-password", map[string]string{"username": "unicode", "password": "🔐🔐🔐🔐🔐🔐🔐🔐"}); w.Code != 204 {
		t.Fatalf("eight-character reset %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "unicode", "password": "🔐🔐🔐🔐🔐🔐🔐🔐"}); w.Code != 200 {
		t.Fatalf("reset login %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/auth/local/login", map[string]string{"username": "alice", "password": "abcdefgh"}); w.Code != 200 {
		t.Fatalf("reset login %d %s", w.Code, w.Body.String())
	}
}

func TestUsernamePasswordChangeEightCharacterBoundary(t *testing.T) {
	pool, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	registration, login, _ := usernameServices(t, pool)
	now := time.Now().UTC()
	if _, err := registration.RegisterUsername(ctx, "boundary-owner", "12345678", "192.0.2.1", now); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM auth_password_credentials WHERE username_normalized='boundary-owner'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := login.ChangePassword(ctx, userID, "12345678", "1234567", "192.0.2.2", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("seven-character change status: %v", err)
	}
	if err := login.ChangePassword(ctx, userID, "12345678", "ééééééé", "192.0.2.3", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("seven-character Unicode change status: %v", err)
	}
	if err := login.ChangePassword(ctx, userID, "12345678", "éééééééé", "192.0.2.3", now); err != nil {
		t.Fatalf("eight-character change rejected: %v", err)
	}
	if _, err := login.Login(ctx, "boundary-owner", "12345678", "192.0.2.4", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old password accepted: %v", err)
	}
	if _, err := login.Login(ctx, "boundary-owner", "éééééééé", "192.0.2.5", now); err != nil {
		t.Fatalf("changed password login rejected: %v", err)
	}
}

func TestUsernameResetRevokesSessionsAndThrottles(t *testing.T) {
	pool, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	registration, login, refresh := usernameServices(t, pool)
	now := time.Now().UTC()
	if _, err := registration.RegisterUsername(ctx, "Owner.Name", "original long password", "192.0.2.1", now); err != nil {
		t.Fatal(err)
	}
	old, err := login.Login(ctx, "owner.name", "original long password", "192.0.2.2", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := login.ResetUsernamePassword(ctx, " OWNER.NAME ", "new secure password", "192.0.2.3", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalAccessVerifier{DB: pool, Codec: login.Codec}).Verify(ctx, old.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old access remained valid: %v", err)
	}
	if _, err := refresh.Rotate(ctx, old.RefreshToken, now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old refresh remained valid: %v", err)
	}
	if _, err := login.Login(ctx, "owner.name", "original long password", "192.0.2.4", now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old password remained valid: %v", err)
	}
	if _, err := login.Login(ctx, "owner.name", "new secure password", "192.0.2.5", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("new password login: %v", err)
	}
	if err := login.ResetUsernamePassword(ctx, "unknown.name", "another long password", "192.0.2.6", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("unknown account response: %v", err)
	}
	for _, password := range []string{"second secure password", "third secure password", "fourth secure password"} {
		if err := login.ResetUsernamePassword(ctx, "owner.name", password, "192.0.2.7", now.Add(4*time.Minute)); err != nil {
			t.Fatalf("reset response: %v", err)
		}
	}
	if _, err := login.Login(ctx, "owner.name", "third secure password", "192.0.2.8", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("last allowed password: %v", err)
	}
	if _, err := login.Login(ctx, "owner.name", "fourth secure password", "192.0.2.9", now.Add(5*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("throttled reset changed password: %v", err)
	}
}
