package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnlockDevUsernameLoginOnlyExhaustedApprovedAccountBucket(t *testing.T) {
	db, _ := usernameTestDatabase(t, false)
	ctx := context.Background()
	email := "approved-dev@example.test"
	if _, err := db.Exec(ctx, `INSERT INTO users(id,email,email_verified) VALUES($1,$2,true)`, DevLocalUserID, email); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, DevLocalUserID, DevLocalIssuer, email); err != nil {
		t.Fatal(err)
	}
	if err := AdoptDevAccountUsername(ctx, db, DevLocalUserID, "dev", "correct long password", time.Now()); err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	accountKey := (LocalLoginService{RateKey: key}).rateHash("account", "username_local:dev")
	sourceKey := (LocalLoginService{RateKey: key}).rateHash("source", "192.0.2.1")
	for _, bucket := range []struct {
		scope    string
		key      []byte
		attempts int
	}{
		{"account_hour", accountKey, 9}, {"source_hour", sourceKey, 4},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO auth_login_rate_limits(scope,key_hash,window_start,attempts) VALUES($1,$2,now(),$3)`, bucket.scope, bucket.key, bucket.attempts); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []struct {
		id, user string
		key      []byte
	}{
		{"00000000-0000-4000-8000-000000000000", "dev", key},
		{DevLocalUserID, "other", key},
		{DevLocalUserID, "dev", []byte("abcdef0123456789abcdef0123456789")},
	} {
		if err := UnlockDevUsernameLogin(ctx, db, input.key, input.id, input.user); !errors.Is(err, ErrDevLoginUnlockConflict) {
			t.Fatalf("invalid unlock accepted: %v", err)
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM auth_identities WHERE user_id=$1 AND issuer=$2`, DevLocalUserID, LocalUsernameIssuer); err != nil {
		t.Fatal(err)
	}
	if err := UnlockDevUsernameLogin(ctx, db, key, DevLocalUserID, "dev"); !errors.Is(err, ErrDevLoginUnlockConflict) {
		t.Fatalf("missing identity accepted: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$1::uuid::text)`, DevLocalUserID, LocalUsernameIssuer); err != nil {
		t.Fatal(err)
	}
	if err := UnlockDevUsernameLogin(ctx, db, key, DevLocalUserID, "dev"); err != nil {
		t.Fatal(err)
	}
	var count, attempts int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM auth_login_rate_limits WHERE scope='account_hour' AND key_hash=$1`, accountKey).Scan(&count); err != nil || count != 0 {
		t.Fatalf("account bucket retained: %d %v", count, err)
	}
	if err := db.QueryRow(ctx, `SELECT attempts FROM auth_login_rate_limits WHERE scope='source_hour' AND key_hash=$1`, sourceKey).Scan(&attempts); err != nil || attempts != 4 {
		t.Fatalf("source bucket changed: %d %v", attempts, err)
	}
	if err := UnlockDevUsernameLogin(ctx, db, key, DevLocalUserID, "dev"); !errors.Is(err, ErrDevLoginUnlockConflict) {
		t.Fatalf("repeat unlock accepted: %v", err)
	}
}
