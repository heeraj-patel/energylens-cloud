package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func sessionTestPool(t *testing.T) (*pgxpool.Pool, *pgxpool.Config) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_session_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, cfg
}

func waitForFamilyLockWaiters(t *testing.T, pool *pgxpool.Pool, holderPID, want int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks waiting
			JOIN pg_locks held ON held.locktype='advisory' AND held.granted AND held.pid=$1
			AND waiting.locktype=held.locktype AND waiting.database=held.database
			AND waiting.classid=held.classid AND waiting.objid=held.objid AND waiting.objsubid=held.objsubid
			WHERE waiting.granted=false`, holderPID).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("family lock did not have %d waiting transaction(s)", want)
}

func usernameSessionFixture(t *testing.T, pool *pgxpool.Pool) (string, LocalAccessCodec, LocalLoginService, LocalRefreshService) {
	t.Helper()
	ctx := context.Background()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "test-key", Keys: map[string]ed25519.PublicKey{"test-key": public}, Audience: "local-test-audience", Profile: LocalUsernameProfile}
	login := LocalLoginService{DB: pool, Codec: codec, RateKey: []byte(strings.Repeat("s", 32))}
	refresh := LocalRefreshService{DB: pool, Codec: codec}
	userID := uuid.NewString()
	hash, err := HashLocalPassword("original long password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,email_verified) VALUES($1,false)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$1::uuid::text)`, userID, LocalUsernameIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,credential_kind,username_normalized,activated_at,password_hash)
		VALUES($1,'username_local','session-owner',now(),$2)`, userID, hash); err != nil {
		t.Fatal(err)
	}
	return userID, codec, login, refresh
}

func resignSession(t *testing.T, codec LocalAccessCodec, subject, sid, issuer, audience string, version int64) string {
	t.Helper()
	now := time.Now().UTC()
	claims := localAccessClaims{Version: version, AuthTime: jwt.NewNumericDate(now), SessionID: sid, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: subject, Audience: jwt.ClaimStrings{audience}, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(localAccessLifetime)),
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = codec.ActiveKeyID
	raw, err := tok.SignedString(codec.Private)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUsernameSessionFamiliesSurviveRestartAndRevokeIndependently(t *testing.T) {
	pool, cfg := sessionTestPool(t)
	userID, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.1", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.2", now)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(raw string, wantValid bool) Identity {
		t.Helper()
		id, e := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, raw)
		if (e == nil) != wantValid {
			t.Fatalf("access validity = %v, wanted %v: %v", e == nil, wantValid, e)
		}
		return id
	}
	firstID := verify(first.AccessToken, true)
	secondID := verify(second.AccessToken, true)
	if firstID.SessionID == "" || firstID.SessionID == secondID.SessionID || firstID.Subject != userID || firstID.EmailVerified {
		t.Fatal("username session identity or family incorrect")
	}
	rotated, err := refresh.Rotate(ctx, first.RefreshToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if verify(rotated.AccessToken, true).SessionID != firstID.SessionID {
		t.Fatal("rotation changed session ID")
	}
	verify(first.AccessToken, true) // A rotated predecessor is revoked, but its family lives.
	restartDir := os.Getenv("ENERGYLENS_CLOUD_TEST_POSTGRES_DATA_DIR")
	if restartDir != "" {
		var actualDir string
		if err = pool.QueryRow(ctx, `SHOW data_directory`).Scan(&actualDir); err != nil {
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
			t.Fatalf("refusing to restart unexpected PostgreSQL data directory")
		}
	}
	pool.Close()
	if restartDir != "" {
		restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(restartCtx, "pg_ctl", "-D", restartDir,
			"-l", filepath.Join(restartDir, "local-account-test-restart.log"), "-m", "fast", "-w", "restart")
		cmd.WaitDelay = 5 * time.Second
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("disposable PostgreSQL restart failed: %v: %s", err, strings.TrimSpace(string(output)))
		}
	}
	reopened, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool = reopened
	t.Cleanup(reopened.Close)
	refresh.DB = reopened
	verify(rotated.AccessToken, true)
	if err = refresh.Logout(ctx, first.RefreshToken, now); err != nil {
		t.Fatal(err)
	}
	verify(first.AccessToken, false)
	verify(rotated.AccessToken, false)
	verify(second.AccessToken, true)
	if _, err = pool.Exec(ctx, `UPDATE auth_refresh_sessions SET created_at=now()-interval '2 hours',
		expires_at=now()-interval '1 hour' WHERE family_id=$1`, secondID.SessionID); err != nil {
		t.Fatal(err)
	}
	verify(second.AccessToken, false)
	if _, err = refresh.Rotate(ctx, rotated.RefreshToken, now.Add(3*time.Second)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("logged-out successor refreshed: %v", err)
	}
	if err = refresh.Logout(ctx, first.RefreshToken, now.Add(3*time.Second)); err != nil {
		t.Fatalf("logout lost idempotency: %v", err)
	}
}

func TestUsernameSessionRejectsSignedWrongClaimsAndCrossProfileMutation(t *testing.T) {
	pool, _ := sessionTestPool(t)
	userID, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tokens, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.3", now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"missing family": resignSession(t, codec, userID, "", LocalUsernameIssuer, codec.Audience, 1),
		"foreign family": resignSession(t, codec, userID, uuid.NewString(), LocalUsernameIssuer, codec.Audience, 1),
		"foreign user":   resignSession(t, codec, uuid.NewString(), id.SessionID, LocalUsernameIssuer, codec.Audience, 1),
		"old version":    resignSession(t, codec, userID, id.SessionID, LocalUsernameIssuer, codec.Audience, 2),
		"wrong issuer":   resignSession(t, codec, userID, id.SessionID, LocalIssuer, codec.Audience, 1),
		"wrong audience": resignSession(t, codec, userID, id.SessionID, LocalUsernameIssuer, "foreign-audience", 1),
	} {
		if _, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, raw); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	expired, err := codec.signVerifiedSessionAt(userID, 1, id.SessionID, now.Add(-time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, expired); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expired access accepted: %v", err)
	}
	emailCodec := codec
	emailCodec.Profile = LocalEmailProfile
	wrongProfile := LocalRefreshService{DB: pool, Codec: emailCodec}
	if err := wrongProfile.Logout(ctx, tokens.RefreshToken, now.Add(time.Second)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("cross-profile logout: %v", err)
	}
	if _, err := wrongProfile.Rotate(ctx, tokens.RefreshToken, now.Add(time.Second)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("cross-profile rotation: %v", err)
	}
	if _, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken); err != nil {
		t.Fatalf("cross-profile mutation revoked family: %v", err)
	}
	rotated, err := refresh.Rotate(ctx, tokens.RefreshToken, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refresh.Rotate(ctx, tokens.RefreshToken, now.Add(2*time.Second)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("replay accepted: %v", err)
	}
	if _, err = (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("replay left access valid: %v", err)
	}
}

func TestUsernameRefreshLogoutRaceCannotResurrectAccess(t *testing.T) {
	pool, _ := sessionTestPool(t)
	_, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tokens, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.4", now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	var holderPID int
	if err = gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, id.SessionID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var rotated LocalLoginTokens
	var rotateErr, logoutErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		rotated, rotateErr = refresh.Rotate(ctx, tokens.RefreshToken, now.Add(time.Second))
	}()
	go func() { defer wg.Done(); logoutErr = refresh.Logout(ctx, tokens.RefreshToken, now.Add(time.Second)) }()
	waitForFamilyLockWaiters(t, pool, holderPID, 2)
	if err = gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if logoutErr != nil || rotateErr != nil && !errors.Is(rotateErr, ErrInvalidCredential) {
		t.Fatalf("race: rotate=%v logout=%v", rotateErr, logoutErr)
	}
	verifier := LocalAccessVerifier{DB: pool, Codec: codec}
	if _, err = verifier.Verify(ctx, tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old access resurrected: %v", err)
	}
	if rotated.AccessToken != "" {
		if _, err = verifier.Verify(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("rotated access resurrected: %v", err)
		}
	}
}

func TestUsernamePasswordChangeRevokesConcurrentLoginAndAllFamilies(t *testing.T) {
	pool, _ := sessionTestPool(t)
	userID, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.11", now)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, first.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	var holderPID int
	if err = gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, firstID.SessionID); err != nil {
		t.Fatal(err)
	}
	changeDone := make(chan error, 1)
	go func() {
		changeDone <- login.ChangePassword(ctx, userID, "original long password", "replacement long password", "192.0.2.12", now)
	}()
	waitForFamilyLockWaiters(t, pool, holderPID, 1)
	// Login is allowed while the existing family lock is held. The password
	// change must detect and revoke this newly inserted family on retry.
	concurrent, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.13", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-changeDone; err != nil {
		t.Fatal(err)
	}
	verifier := LocalAccessVerifier{DB: pool, Codec: codec}
	for _, raw := range []string{first.AccessToken, concurrent.AccessToken} {
		if _, err = verifier.Verify(ctx, raw); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("pre-change access survived: %v", err)
		}
	}
	for _, raw := range []string{first.RefreshToken, concurrent.RefreshToken} {
		if _, err = refresh.Rotate(ctx, raw, now); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("pre-change refresh survived: %v", err)
		}
	}
	if _, err = login.Login(ctx, "session-owner", "original long password", "192.0.2.14", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old password accepted: %v", err)
	}
	replacement, err := login.Login(ctx, "session-owner", "replacement long password", "192.0.2.15", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(ctx, replacement.AccessToken); err != nil {
		t.Fatalf("replacement access denied: %v", err)
	}
}

func TestUsernamePasswordChangeAndRefreshRaceCannotRetainFamily(t *testing.T) {
	pool, _ := sessionTestPool(t)
	userID, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tokens, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.16", now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	var holderPID int
	if err = gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, id.SessionID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var rotated LocalLoginTokens
	var rotateErr, changeErr error
	wg.Add(2)
	go func() { defer wg.Done(); rotated, rotateErr = refresh.Rotate(ctx, tokens.RefreshToken, now) }()
	go func() {
		defer wg.Done()
		changeErr = login.ChangePassword(ctx, userID, "original long password", "replacement long password", "192.0.2.17", now)
	}()
	waitForFamilyLockWaiters(t, pool, holderPID, 2)
	if err = gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if changeErr != nil || rotateErr != nil && !errors.Is(rotateErr, ErrInvalidCredential) {
		t.Fatalf("password/refresh race: change=%v refresh=%v", changeErr, rotateErr)
	}
	verifier := LocalAccessVerifier{DB: pool, Codec: codec}
	if _, err = verifier.Verify(ctx, tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old access survived password change: %v", err)
	}
	if rotated.AccessToken != "" {
		if _, err = verifier.Verify(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("rotated access survived password change: %v", err)
		}
	}
	if _, err = refresh.Rotate(ctx, tokens.RefreshToken, now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old refresh survived password change: %v", err)
	}
	if rotated.RefreshToken != "" {
		if _, err = refresh.Rotate(ctx, rotated.RefreshToken, now); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("rotated refresh survived password change: %v", err)
		}
	}
}

func TestUsernameDeletionRequiresRecentAuthAndTombstonesSession(t *testing.T) {
	pool, _ := sessionTestPool(t)
	userID, codec, login, refresh := usernameSessionFixture(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tokens, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.21", now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := codec.signVerifiedSessionAt(userID, 1, id.SessionID, now, now.Add(-6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	router := chi.NewRouter()
	router.Route("/v1", func(r chi.Router) {
		(Service{Store: store, Deletion: store, Verifier: LocalAccessVerifier{DB: pool, Codec: codec}}).Routes(r)
	})
	deleteWith := func(raw string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	if code := deleteWith(stale); code != http.StatusForbidden {
		t.Fatalf("stale sign-in deleted account: %d", code)
	}
	if code := deleteWith(tokens.AccessToken); code != http.StatusNoContent {
		t.Fatalf("fresh sign-in failed to delete: %d", code)
	}
	if _, err = (LocalAccessVerifier{DB: pool, Codec: codec}).Verify(ctx, tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("deleted access survived: %v", err)
	}
	if _, err = refresh.Rotate(ctx, tokens.RefreshToken, now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("deleted refresh survived: %v", err)
	}
	if err = refresh.Logout(ctx, tokens.RefreshToken, now); err != nil {
		t.Fatalf("deleted account logout lost idempotency: %v", err)
	}
	if _, err = login.Login(ctx, "session-owner", "original long password", "192.0.2.22", now); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("deleted username logged in: %v", err)
	}
	var deletedAt *time.Time
	if err = pool.QueryRow(ctx, `SELECT deleted_at FROM users WHERE id=$1`, userID).Scan(&deletedAt); err != nil || deletedAt == nil {
		t.Fatalf("deletion tombstone missing: %v", err)
	}
}
