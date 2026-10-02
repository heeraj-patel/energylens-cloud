package account

import (
	"context"
	"errors"
	"fmt"
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

type capturedVerification struct {
	emails []string
	tokens []string
	fail   bool
}

func (s *capturedVerification) SendVerification(_ context.Context, email, token string) error {
	s.emails = append(s.emails, email)
	s.tokens = append(s.tokens, token)
	if s.fail {
		return errors.New("simulated sender failure containing private data")
	}
	return nil
}

func TestLocalRegistrationVerificationRateAndTombstone(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "registration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	sender := &capturedVerification{fail: true}
	service := LocalRegistrationService{DB: pool, Sender: sender, RateKey: []byte(strings.Repeat("k", 32))}
	now := time.Now().UTC()
	request := func(email, password, source string, at time.Time) RegistrationAccepted {
		t.Helper()
		result, requestErr := service.RequestRegistration(ctx, email, password, source, at)
		if requestErr != nil || result.Status != "accepted" {
			t.Fatalf("registration response: %+v %v", result, requestErr)
		}
		return result
	}
	first := request("Person@Example.com", "correct long password", "192.0.2.1", now)
	if len(sender.tokens) != 1 || sender.emails[0] != "person@example.com" {
		t.Fatalf("new registration delivery: %+v", sender)
	}
	var userID, passwordHash string
	var verified bool
	var profileEmail *string
	if err = pool.QueryRow(ctx, `SELECT c.user_id,c.password_hash,u.email,u.email_verified FROM auth_password_credentials c JOIN users u ON u.id=c.user_id WHERE c.email_normalized='person@example.com'`).Scan(&userID, &passwordHash, &profileEmail, &verified); err != nil {
		t.Fatal(err)
	}
	if profileEmail != nil || verified || !VerifyLocalPassword(passwordHash, "correct long password") {
		t.Fatal("unverified registration exposed profile email or lost password hash")
	}
	var identityUser string
	if err = pool.QueryRow(ctx, `SELECT user_id FROM auth_identities WHERE issuer=$1 AND subject=$2`, LocalIssuer, userID).Scan(&identityUser); err != nil || identityUser != userID {
		t.Fatalf("stable local identity: %q %v", identityUser, err)
	}
	sender.fail = false
	second := request("person@example.com", "different long password", "192.0.2.1", now.Add(time.Minute))
	if second != first || len(sender.tokens) != 2 || VerifyLocalPassword(passwordHash, "different long password") {
		t.Fatal("existing unverified address response or password changed")
	}
	if err = service.VerifyEmail(ctx, sender.tokens[0], now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("superseded challenge accepted: %v", err)
	}
	request("person@example.com", "different long password", "192.0.2.1", now.Add(2*time.Minute))
	if len(sender.tokens) != 3 {
		t.Fatal("third bounded verification request was not sent")
	}
	request("person@example.com", "different long password", "192.0.2.1", now.Add(3*time.Minute))
	if len(sender.tokens) != 3 {
		t.Fatal("fourth email request bypassed daily rate bound")
	}
	if err = service.VerifyEmail(ctx, sender.tokens[2], now.Add(4*time.Minute)); err != nil {
		t.Fatalf("current challenge rejected: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT email,email_verified FROM users WHERE id=$1`, userID).Scan(&profileEmail, &verified); err != nil || profileEmail == nil || *profileEmail != "person@example.com" || !verified {
		t.Fatalf("verification did not update profile: %v %v %v", profileEmail, verified, err)
	}
	if err = service.VerifyEmail(ctx, sender.tokens[2], now.Add(5*time.Minute)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("replayed challenge accepted: %v", err)
	}
	request("person@example.com", "another long password", "192.0.2.2", now.Add(25*time.Hour))
	if len(sender.tokens) != 3 {
		t.Fatal("verified account received another signup challenge")
	}
	// External OIDC identity with the same email remains a distinct subject.
	oidcUser, err := (PGStore{Pool: pool}).Resolve(ctx, Identity{Issuer: "https://id.example", Subject: "person", Email: "person@example.com", EmailVerified: true})
	if err != nil || oidcUser.ID == userID {
		t.Fatalf("OIDC identity merged by email: %+v %v", oidcUser, err)
	}
	if err = (PGStore{Pool: pool}).DeleteAccount(ctx, userID); err != nil {
		t.Fatal(err)
	}
	request("person@example.com", "another long password", "192.0.2.2", now.Add(26*time.Hour))
	if len(sender.tokens) != 3 {
		t.Fatal("tombstoned local identity received a new challenge")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE issuer=$1 AND subject=$2`, LocalIssuer, userID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("tombstone lost stable identity: %d %v", count, err)
	}
	beforeSourceRate := len(sender.tokens)
	for i := 0; i < 11; i++ {
		request(fmt.Sprintf("new%d@example.com", i), "another long password", "192.0.2.99", now.Add(27*time.Hour))
	}
	if len(sender.tokens) != beforeSourceRate+10 {
		t.Fatalf("source hourly rate did not cap delivery: %d", len(sender.tokens)-beforeSourceRate)
	}
}
