package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

func TestLocalEmailChangeUpdatesCanonicalEmailAndReplays(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "change_email_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	login := LocalLoginService{DB: pool, Codec: LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}, RateKey: []byte(strings.Repeat("l", 32))}
	now := time.Now().UTC()
	password := "original long password"
	addUser := func(email string) string {
		t.Helper()
		id := uuid.NewString()
		hash, hashErr := HashLocalPassword(password)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,email_verified) VALUES($1,$2,true)`, id, email); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, id, LocalIssuer, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash,verified_at) VALUES($1,$2,$3,$4)`, id, email, hash, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := addUser("person@example.test")
	_ = addUser("taken@example.test")
	requestHash := []byte(strings.Repeat("h", 32))
	key := uuid.NewString()
	if _, err := login.ChangeEmail(ctx, owner, "new.person@example.test", "wrong long password", "192.0.2.1", 1, key, requestHash, now); err != ErrInvalidCredential {
		t.Fatalf("wrong current password accepted: %v", err)
	}
	if _, err := login.ChangeEmail(ctx, owner, "taken@example.test", password, "192.0.2.1", 1, key, requestHash, now); err != ErrEmailInUse {
		t.Fatalf("duplicate address result: %v", err)
	}
	updated, err := login.ChangeEmail(ctx, owner, "new.person@example.test", password, "192.0.2.1", 1, key, requestHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Email == nil || *updated.Email != "new.person@example.test" || !updated.EmailVerified || updated.ProfileVersion != 2 {
		t.Fatalf("unexpected profile result: %+v", updated)
	}
	replayed, err := login.ChangeEmail(ctx, owner, "new.person@example.test", "any password", "192.0.2.1", 1, key, requestHash, now.Add(time.Second))
	if err != nil || replayed.ProfileVersion != 2 || replayed.Email == nil || *replayed.Email != "new.person@example.test" {
		t.Fatalf("email change replay: %+v %v", replayed, err)
	}
	if _, err = login.ChangeEmail(ctx, owner, "other@example.test", password, "192.0.2.1", 1, uuid.NewString(), requestHash, now); err != ErrVersion {
		t.Fatalf("stale profile version accepted: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT c.email_normalized FROM auth_password_credentials c WHERE c.user_id=$1`, owner).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "new.person@example.test" {
		t.Fatalf("login email not updated: %s", stored)
	}
	if _, err := login.Login(ctx, "person@example.test", password, "192.0.2.1", now.Add(time.Minute)); err != ErrInvalidCredential {
		t.Fatalf("old login address still works: %v", err)
	}
	if _, err := login.Login(ctx, "NEW.PERSON@example.test", password, "192.0.2.2", now.Add(time.Minute)); err != nil {
		t.Fatalf("new login address failed: %v", err)
	}
}
