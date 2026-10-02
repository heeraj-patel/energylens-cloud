package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
	service := LocalLoginService{DB: pool, Codec: LocalAccessCodec{Private: private, Public: public, Audience: "energylens-cloud"}, RateKey: []byte(strings.Repeat("r", 32))}
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
	identity, err := service.Codec.Verify(ctx, tokens.AccessToken)
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
