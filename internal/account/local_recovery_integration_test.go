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

type capturedRecovery struct {
	emails, tokens []string
	fail           bool
}

func (s *capturedRecovery) SendRecovery(_ context.Context, email, token string) error {
	s.emails = append(s.emails, email)
	s.tokens = append(s.tokens, token)
	if s.fail {
		return errors.New("private delivery error")
	}
	return nil
}

func TestLocalRecoveryResetsAndRevokesSessions(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recovery_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	now := time.Now().UTC()
	userID, unverifiedID, deletedID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}
	verifier := LocalAccessVerifier{DB: pool, Codec: codec}
	oldHash, err := HashLocalPassword("original long password")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id, name string
		verified bool
		deleted  bool
	}{
		{userID, "person", true, false}, {unverifiedID, "unverified", false, false}, {deletedID, "deleted", true, true},
	} {
		var deletedAt, verifiedAt any
		if fixture.deleted {
			deletedAt = now
		}
		if fixture.verified {
			verifiedAt = now
		}
		if _, err = pool.Exec(ctx, `INSERT INTO users(id,email,email_verified,deleted_at) VALUES($1,$2,$3,$4)`, fixture.id, fixture.name+"@example.com", fixture.verified, deletedAt); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, fixture.id, LocalIssuer, fixture.id); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash,verified_at) VALUES($1,$2,$3,$4)`, fixture.id, fixture.name+"@example.com", oldHash, verifiedAt); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{unverifiedID, deletedID} {
		access, signErr := codec.signVerified(id, 1, now)
		if signErr != nil {
			t.Fatal(signErr)
		}
		if _, verifyErr := verifier.Verify(ctx, access); !errors.Is(verifyErr, ErrInvalidCredential) {
			t.Fatalf("ineligible local identity accepted: %v", verifyErr)
		}
	}
	sender := &capturedRecovery{fail: true}
	svc := LocalRecoveryService{DB: pool, Sender: sender, RateKey: []byte(strings.Repeat("r", 32))}
	request := func(email, source string, at time.Time) {
		t.Helper()
		result, requestErr := svc.RequestReset(ctx, email, source, at)
		if requestErr != nil || result.Status != "accepted" {
			t.Fatalf("recovery result: %+v %v", result, requestErr)
		}
	}
	request("unknown@example.com", "192.0.2.1", now)
	request("unverified@example.com", "192.0.2.1", now)
	request("deleted@example.com", "192.0.2.1", now)
	if len(sender.tokens) != 0 {
		t.Fatal("ineligible recovery delivered")
	}
	request("PERSON@example.com", "192.0.2.1", now)
	if len(sender.tokens) != 1 || sender.emails[0] != "person@example.com" {
		t.Fatal("recovery not delivered")
	}
	first := sender.tokens[0]
	sender.fail = false
	request("person@example.com", "192.0.2.1", now.Add(time.Minute))
	if err = svc.ResetPassword(ctx, first, "new secure password", now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("superseded token accepted: %v", err)
	}
	second := sender.tokens[1]
	request("person@example.com", "192.0.2.1", now.Add(2*time.Minute))
	request("person@example.com", "192.0.2.1", now.Add(3*time.Minute))
	if len(sender.tokens) != 3 {
		t.Fatal("daily email rate bypassed")
	}
	if err = svc.ResetPassword(ctx, second, "new secure password", now.Add(4*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("superseded challenge accepted: %v", err)
	}
	current := sender.tokens[2]
	if err = svc.ResetPassword(ctx, current, "short", now.Add(5*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("weak password accepted: %v", err)
	}
	_, refreshHash, err := NewLocalOpaqueToken("refresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_refresh_sessions(user_id,family_id,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5)`, userID, uuid.NewString(), refreshHash[:], now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	access, err := codec.signVerified(userID, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(ctx, access); err != nil {
		t.Fatalf("pre-reset access rejected: %v", err)
	}
	if err = svc.ResetPassword(ctx, current, "new secure password", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(ctx, access); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("pre-reset access survived: %v", err)
	}
	newAccess, err := codec.signVerified(userID, 2, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(ctx, newAccess); err != nil {
		t.Fatalf("new-version access rejected: %v", err)
	}
	var stored string
	var active int
	if err = pool.QueryRow(ctx, `SELECT password_hash FROM auth_password_credentials WHERE user_id=$1`, userID).Scan(&stored); err != nil || !VerifyLocalPassword(stored, "new secure password") || VerifyLocalPassword(stored, "original long password") {
		t.Fatalf("password not replaced: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_refresh_sessions WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("reset left active sessions: %d %v", active, err)
	}
	if err = svc.ResetPassword(ctx, current, "another long password", now.Add(6*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("replayed reset accepted: %v", err)
	}
	request("person@example.com", "192.0.2.2", now.Add(25*time.Hour))
	if len(sender.tokens) != 4 {
		t.Fatal("next-day recovery was not delivered")
	}
	if err = svc.ResetPassword(ctx, sender.tokens[3], "another long password", now.Add(26*time.Hour)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expired token accepted: %v", err)
	}
	for i := 0; i < 11; i++ {
		request("unknown"+string(rune('a'+i))+"@example.com", "192.0.2.99", now.Add(27*time.Hour))
	}
	var attempts int
	if err = pool.QueryRow(ctx, `SELECT attempts FROM auth_password_reset_rate_limits WHERE scope='source_hour' AND key_hash=$1`, svc.rateHash("source", "192.0.2.99")).Scan(&attempts); err != nil || attempts != 11 {
		t.Fatalf("source rate not counted: %d %v", attempts, err)
	}
}
