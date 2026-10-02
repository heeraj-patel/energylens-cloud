package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalLoginVerifiedOnlyGenericFailuresAndHashedRefresh(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_login_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	service := LocalLoginService{DB: pool, Codec: LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}, RateKey: []byte(strings.Repeat("r", 32))}
	now := time.Now().UTC()
	passwordHash, err := HashLocalPassword("correct long password")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"verified", "unverified", "deleted"} {
		id := uuid.NewString()
		ids[name] = id
		if _, err = pool.Exec(ctx, `INSERT INTO users(id,email,email_verified,deleted_at) VALUES($1,$2,$3,$4)`, id, name+"@example.com", name != "unverified", func() any {
			if name == "deleted" {
				return now
			}
			return nil
		}()); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, id, LocalIssuer, id); err != nil {
			t.Fatal(err)
		}
		var verifiedAt any
		if name != "unverified" {
			verifiedAt = now
		}
		if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash,verified_at) VALUES($1,$2,$3,$4)`, id, name+"@example.com", passwordHash, verifiedAt); err != nil {
			t.Fatal(err)
		}
	}
	denied := func(email, password, source string) {
		t.Helper()
		tokens, loginErr := service.Login(ctx, email, password, source, now)
		if !errors.Is(loginErr, ErrInvalidCredential) || tokens.AccessToken != "" || tokens.RefreshToken != "" {
			t.Fatalf("login failure differs: %+v %v", tokens, loginErr)
		}
	}
	denied("nobody@example.com", "correct long password", "192.0.2.1")
	denied("verified@example.com", "wrong long password", "192.0.2.2")
	denied("unverified@example.com", "correct long password", "192.0.2.3")
	denied("deleted@example.com", "correct long password", "192.0.2.4")
	tokens, err := service.Login(ctx, "VERIFIED@EXAMPLE.COM", "correct long password", "192.0.2.5", now)
	if err != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.AccessExpiresAt.Sub(now) != localAccessLifetime || tokens.RefreshExpiresAt.Sub(now) != localRefreshLifetime {
		t.Fatalf("verified login: %+v %v", tokens, err)
	}
	identity, err := (LocalAccessVerifier{DB: pool, Codec: service.Codec}).Verify(ctx, tokens.AccessToken)
	if err != nil || identity.Issuer != LocalIssuer || identity.Subject != ids["verified"] {
		t.Fatalf("local access identity: %+v %v", identity, err)
	}
	hash, err := HashLocalOpaqueToken("refresh", tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err = pool.QueryRow(ctx, `SELECT token_hash FROM auth_refresh_sessions WHERE user_id=$1`, ids["verified"]).Scan(&stored); err != nil || string(stored) != string(hash[:]) {
		t.Fatalf("refresh session hash mismatch: %v", err)
	}
	refresh := LocalRefreshService{DB: pool, Codec: service.Codec}
	rotated, err := refresh.Rotate(ctx, tokens.RefreshToken, now.Add(time.Minute))
	if err != nil || rotated.RefreshToken == "" || rotated.RefreshToken == tokens.RefreshToken || rotated.RefreshExpiresAt.Sub(tokens.RefreshExpiresAt).Abs() > time.Microsecond {
		t.Fatalf("refresh rotation: %+v %v", rotated, err)
	}
	if _, err = refresh.Rotate(ctx, tokens.RefreshToken, now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("rotated token replay accepted: %v", err)
	}
	if _, err = refresh.Rotate(ctx, rotated.RefreshToken, now.Add(3*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("replay failed to revoke family: %v", err)
	}
	secondFamily, err := service.Login(ctx, "verified@example.com", "correct long password", "192.0.2.50", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = refresh.Logout(ctx, secondFamily.RefreshToken, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = refresh.Logout(ctx, secondFamily.RefreshToken, now.Add(time.Minute)); err != nil {
		t.Fatalf("logout was not idempotent: %v", err)
	}
	if _, err = refresh.Rotate(ctx, secondFamily.RefreshToken, now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("logged-out token accepted: %v", err)
	}
	thirdFamily, err := service.Login(ctx, "verified@example.com", "correct long password", "192.0.2.51", now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rotateErr := refresh.Rotate(ctx, thirdFamily.RefreshToken, now.Add(time.Minute))
			results <- rotateErr
		}()
	}
	wg.Wait()
	close(results)
	successes, denials := 0, 0
	for rotateErr := range results {
		if rotateErr == nil {
			successes++
		} else if errors.Is(rotateErr, ErrInvalidCredential) {
			denials++
		} else {
			t.Fatalf("concurrent rotation error: %v", rotateErr)
		}
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("concurrent rotation outcomes: success=%d denial=%d", successes, denials)
	}
	var active int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_refresh_sessions WHERE user_id=$1 AND revoked_at IS NULL`, ids["verified"]).Scan(&active); err != nil || active != 0 {
		t.Fatalf("replayed family retained active sessions: %d %v", active, err)
	}
	fourthFamily, err := service.Login(ctx, "verified@example.com", "correct long password", "192.0.2.52", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refresh.Rotate(ctx, fourthFamily.RefreshToken, now.Add(localRefreshLifetime)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expired refresh accepted: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE users SET deleted_at=$2 WHERE id=$1`, ids["verified"], now); err != nil {
		t.Fatal(err)
	}
	if _, err = refresh.Rotate(ctx, fourthFamily.RefreshToken, now.Add(time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("deleted account refreshed: %v", err)
	}
	accountKey := service.rateHash("account", "throttle@example.com")
	if _, err = pool.Exec(ctx, `INSERT INTO auth_login_rate_limits(scope,key_hash,window_start,attempts) VALUES('account_hour',$1,$2,4)`, accountKey, now); err != nil {
		t.Fatal(err)
	}
	denied("throttle@example.com", "correct long password", "192.0.2.6")
	denied("throttle@example.com", "correct long password", "192.0.2.7")
	var attempts int
	if err = pool.QueryRow(ctx, `SELECT attempts FROM auth_login_rate_limits WHERE scope='account_hour' AND key_hash=$1`, accountKey).Scan(&attempts); err != nil || attempts != 6 {
		t.Fatalf("account rate not counted: %d %v", attempts, err)
	}
	sourceKey := service.rateHash("source", "192.0.2.99")
	if _, err = pool.Exec(ctx, `INSERT INTO auth_login_rate_limits(scope,key_hash,window_start,attempts) VALUES('source_hour',$1,$2,19)`, sourceKey, now); err != nil {
		t.Fatal(err)
	}
	denied("other@example.com", "correct long password", "192.0.2.99")
	denied("other@example.com", "correct long password", "192.0.2.99")
	if err = pool.QueryRow(ctx, `SELECT attempts FROM auth_login_rate_limits WHERE scope='source_hour' AND key_hash=$1`, sourceKey).Scan(&attempts); err != nil || attempts != 21 {
		t.Fatalf("source rate not counted: %d %v", attempts, err)
	}
}
