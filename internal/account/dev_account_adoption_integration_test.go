package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func devAdoptionFixture(t *testing.T, db *pgxpool.Pool, verified, withIdentity bool) (string, string) {
	t.Helper()
	id, home := uuid.NewString(), uuid.NewString()
	email := id + "@example.test"
	if _, err := db.Exec(context.Background(), `INSERT INTO users(id,email,email_verified) VALUES($1,$2,$3)`, id, email, verified); err != nil {
		t.Fatal(err)
	}
	if withIdentity {
		if _, err := db.Exec(context.Background(), `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, id, DevLocalIssuer, email); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Existing home','America/Chicago')`, home, id); err != nil {
		t.Fatal(err)
	}
	return id, home
}

func TestAdoptDevAccountUsernamePreservesOwnerAndRevocableSession(t *testing.T) {
	db, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	id, home := devAdoptionFixture(t, db, true, true)
	now := time.Now().UTC()
	password := "correct long password"
	if err := AdoptDevAccountUsername(ctx, db, id, "  Owner.Dev  ", password, now); err != nil {
		t.Fatal(err)
	}
	if err := AdoptDevAccountUsername(ctx, db, id, "owner.dev", password, now); err != nil {
		t.Fatalf("same-account retry: %v", err)
	}
	if err := AdoptDevAccountUsername(ctx, db, id, "owner.dev", "different long password", now); !errors.Is(err, ErrDevAccountAdoptionConflict) {
		t.Fatalf("password overwrite accepted: %v", err)
	}
	var owner, username, issuer string
	var retainedEmail string
	var retainedEmailVerified bool
	var credentials, devIdentities, usernameIdentities int
	if err := db.QueryRow(ctx, `SELECT owner_user_id FROM homes WHERE id=$1`, home).Scan(&owner); err != nil || owner != id {
		t.Fatalf("home owner changed: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT email,email_verified FROM users WHERE id=$1`, id).Scan(&retainedEmail, &retainedEmailVerified); err != nil || retainedEmail != id+"@example.test" || !retainedEmailVerified {
		t.Fatalf("verified dev email changed: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT username_normalized FROM auth_password_credentials WHERE user_id=$1`, id).Scan(&username); err != nil || username != "owner.dev" {
		t.Fatalf("username credential missing: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_password_credentials WHERE user_id=$1`, id).Scan(&credentials); err != nil || credentials != 1 {
		t.Fatalf("credential count: %d %v", credentials, err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE user_id=$1 AND issuer=$2`, id, DevLocalIssuer).Scan(&devIdentities); err != nil || devIdentities != 1 {
		t.Fatalf("dev identity changed: %d %v", devIdentities, err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE user_id=$1 AND issuer=$2`, id, LocalUsernameIssuer).Scan(&usernameIdentities); err != nil || usernameIdentities != 1 {
		t.Fatalf("username identity missing: %d %v", usernameIdentities, err)
	}
	_, login, refresh := usernameServices(t, db)
	session, err := login.Login(ctx, "owner.dev", password, "192.0.2.1", now)
	if err != nil {
		t.Fatalf("adopted account login: %v", err)
	}
	identity, err := (LocalAccessVerifier{DB: db, Codec: login.Codec}).Verify(ctx, session.AccessToken)
	if err != nil || identity.Subject != id || identity.SessionID == "" || identity.Issuer != LocalUsernameIssuer {
		t.Fatalf("wrong session identity: %v", err)
	}
	account, err := (PGStore{Pool: db}).Resolve(ctx, identity)
	if err != nil || account.ID != id || account.AuthMethod != "username_password" || account.Username == nil || *account.Username != "owner.dev" || !account.EmailVerified {
		t.Fatalf("adopted account profile changed: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT issuer FROM auth_identities WHERE user_id=$1 AND issuer=$2`, id, DevLocalIssuer).Scan(&issuer); err != nil || issuer != DevLocalIssuer {
		t.Fatalf("dev identity lost: %v", err)
	}
	rotated, err := refresh.Rotate(ctx, session.RefreshToken, time.Now().UTC())
	if err != nil {
		t.Fatalf("adopted account refresh: %v", err)
	}
	rotatedIdentity, err := (LocalAccessVerifier{DB: db, Codec: login.Codec}).Verify(ctx, rotated.AccessToken)
	if err != nil || rotatedIdentity.Subject != id || rotatedIdentity.SessionID != identity.SessionID {
		t.Fatalf("refresh changed adopted account or session family: %v", err)
	}
	if err := refresh.Logout(ctx, rotated.RefreshToken, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalAccessVerifier{DB: db, Codec: login.Codec}).Verify(ctx, session.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("logout failed to revoke access: %v", err)
	}
	if _, err := (LocalAccessVerifier{DB: db, Codec: login.Codec}).Verify(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("logout failed to revoke rotated access: %v", err)
	}
}

func TestAdoptDevAccountUsernameRejectsConflictsWithoutPartialWrite(t *testing.T) {
	db, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	now := time.Now().UTC()
	password := "correct long password"
	first, _ := devAdoptionFixture(t, db, true, true)
	if err := AdoptDevAccountUsername(ctx, db, first, "owner.dev", password, now); err != nil {
		t.Fatal(err)
	}
	second, _ := devAdoptionFixture(t, db, true, true)
	if err := AdoptDevAccountUsername(ctx, db, second, "owner.dev", password, now); !errors.Is(err, ErrDevAccountAdoptionConflict) {
		t.Fatalf("username collision accepted: %v", err)
	}
	for _, id := range []string{second} {
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_password_credentials WHERE user_id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial credential after collision: %d %v", count, err)
		}
		if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE user_id=$1 AND issuer=$2`, id, LocalUsernameIssuer).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial identity after collision: %d %v", count, err)
		}
	}
	unverified, _ := devAdoptionFixture(t, db, false, true)
	nonDev, _ := devAdoptionFixture(t, db, true, false)
	deleted, _ := devAdoptionFixture(t, db, true, true)
	if _, err := db.Exec(ctx, `UPDATE users SET deleted_at=now() WHERE id=$1`, deleted); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{unverified, nonDev, deleted} {
		if err := AdoptDevAccountUsername(ctx, db, id, "fresh."+id[:8], password, now); !errors.Is(err, ErrDevAccountAdoptionConflict) {
			t.Fatalf("ineligible dev account adopted: %v", err)
		}
	}
	withCredential, _ := devAdoptionFixture(t, db, true, true)
	hash, err := HashLocalPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash) VALUES($1,'existing@example.test',$2)`, withCredential, hash); err != nil {
		t.Fatal(err)
	}
	if err := AdoptDevAccountUsername(ctx, db, withCredential, "existing.owner", password, now); !errors.Is(err, ErrDevAccountAdoptionConflict) {
		t.Fatalf("existing credential overwritten: %v", err)
	}
}
