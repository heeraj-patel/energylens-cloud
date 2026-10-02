package account

import (
	"context"
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

func TestLocalAuthFoundationStoresHashesAndCascades(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "local_auth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	userID, otherID := uuid.NewString(), uuid.NewString()
	for _, id := range []string{userID, otherID} {
		if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	passwordHash, err := HashLocalPassword("correct long password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash) VALUES($1,'person@example.com',$2)`, userID, passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash) VALUES($1,'person@example.com',$2)`, otherID, passwordHash); err == nil {
		t.Fatal("duplicate local login email accepted")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, userID, LocalIssuer, userID); err != nil {
		t.Fatal(err)
	}
	_, verificationHash, err := NewLocalOpaqueToken("verify_email")
	if err != nil {
		t.Fatal(err)
	}
	_, refreshHash, err := NewLocalOpaqueToken("refresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_email_verification_tokens(token_hash,user_id,expires_at) VALUES($1,$2,$3)`, verificationHash[:], userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_refresh_sessions(user_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, userID, uuid.NewString(), refreshHash[:], time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"auth_password_credentials", "auth_email_verification_tokens", "auth_refresh_sessions", "auth_identities"} {
		var count int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", userID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained deleted user's auth data: %d %v", table, count, err)
		}
	}
}
