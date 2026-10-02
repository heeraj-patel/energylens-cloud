package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLocalPasswordHashAndBounds(t *testing.T) {
	stored, err := HashLocalPassword("correct long password")
	if err != nil || !strings.HasPrefix(stored, "$argon2id$v=19$m=65536,t=3,p=1$") || !VerifyLocalPassword(stored, "correct long password") || VerifyLocalPassword(stored, "wrong long password") {
		t.Fatalf("password hash/verify failed: %v", err)
	}
	second, err := HashLocalPassword("correct long password")
	if err != nil || second == stored {
		t.Fatal("password salt was reused")
	}
	for _, value := range []string{"short", strings.Repeat("a", 1025), "contains\x00nul"} {
		if _, err := HashLocalPassword(value); err == nil || VerifyLocalPassword(stored, value) {
			t.Fatal("invalid password accepted")
		}
	}
	if VerifyLocalPassword(strings.Replace(stored, "m=65536", "m=999999999", 1), "correct long password") {
		t.Fatal("unbounded Argon2 parameters accepted")
	}
}

func TestLocalEmailNormalization(t *testing.T) {
	if got, err := NormalizeLocalEmail("Person+Home@Example.COM"); err != nil || got != "person+home@example.com" {
		t.Fatalf("normalized email: %q %v", got, err)
	}
	for _, raw := range []string{" person@example.com", "person@example.com ", "person@-example.com", "person@example", "person@example.com\nBCC:other@example.com"} {
		if _, err := NormalizeLocalEmail(raw); err == nil {
			t.Fatalf("invalid email accepted: %q", raw)
		}
	}
}

func TestLocalOpaqueTokenPurposeAndHash(t *testing.T) {
	raw, hash, err := NewLocalOpaqueToken("refresh")
	if err != nil || len(raw) != 43 {
		t.Fatalf("refresh token generation: %v", err)
	}
	replayed, err := HashLocalOpaqueToken("refresh", raw)
	if err != nil || replayed != hash {
		t.Fatal("refresh hash changed")
	}
	verificationHash, err := HashLocalOpaqueToken("verify_email", raw)
	if err != nil || verificationHash == hash {
		t.Fatal("token purposes share hashes")
	}
	if _, err := HashLocalOpaqueToken("refresh", raw+"="); err == nil {
		t.Fatal("noncanonical token accepted")
	}
	if _, _, err := NewLocalOpaqueToken("other"); err == nil {
		t.Fatal("unknown token purpose accepted")
	}
}

func TestLocalAccessTokenIssuerAudienceSignatureAndExpiry(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, Public: public, Audience: "energylens-cloud"}
	userID := uuid.NewString()
	token, err := codec.signVerified(userID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := codec.Verify(context.Background(), token)
	if err != nil || identity.Issuer != LocalIssuer || identity.Subject != userID || identity.EmailVerified {
		t.Fatalf("verified token identity: %+v %v", identity, err)
	}
	wrongAudience := codec
	wrongAudience.Audience = "other-service"
	if _, err := wrongAudience.Verify(context.Background(), token); err == nil {
		t.Fatal("wrong audience accepted")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := codec
	wrongKey.Public = otherPublic
	if _, err := wrongKey.Verify(context.Background(), token); err == nil {
		t.Fatal("wrong signature key accepted")
	}
	expired, err := codec.signVerified(userID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(context.Background(), expired); err == nil {
		t.Fatal("expired access token accepted")
	}
	if _, err := codec.signVerified("not-a-user", time.Now()); err == nil {
		t.Fatal("noncanonical subject accepted")
	}
}
