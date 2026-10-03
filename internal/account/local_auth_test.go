package account

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}
	userID := uuid.NewString()
	token, err := codec.signVerified(userID, 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	subject, version, err := codec.verifySignature(token)
	if err != nil || subject != userID || version != 1 {
		t.Fatalf("verified token identity: %q %d %v", subject, version, err)
	}
	wrongAudience := codec
	wrongAudience.Audience = "other-service"
	if _, _, err := wrongAudience.verifySignature(token); err == nil {
		t.Fatal("wrong audience accepted")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := codec
	wrongKey.Keys = map[string]ed25519.PublicKey{"key-1": otherPublic}
	if _, _, err := wrongKey.verifySignature(token); err == nil {
		t.Fatal("wrong signature key accepted")
	}
	expired, err := codec.signVerified(userID, 1, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := codec.verifySignature(expired); err == nil {
		t.Fatal("expired access token accepted")
	}
	if _, err := codec.signVerified("not-a-user", 1, time.Now()); err == nil {
		t.Fatal("noncanonical subject accepted")
	}
}

func TestLocalRefreshCannotRenewSignInTime(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: "key-1", Keys: map[string]ed25519.PublicKey{"key-1": public}, Audience: "energylens-cloud"}
	now := time.Now().UTC().Truncate(time.Second)
	loginAt := now.Add(-10 * time.Minute)
	token, err := codec.signVerifiedAt(uuid.NewString(), 1, now, loginAt)
	if err != nil {
		t.Fatal(err)
	}
	_, _, got, err := codec.verifySignatureWithAuthTime(token)
	if err != nil || !got.Equal(loginAt) {
		t.Fatalf("refresh changed sign-in time: %v %v", got, err)
	}
}

func TestLocalAccessKeyRotationRequiresExplicitTrustedKey(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := LocalAccessCodec{Private: oldPrivate, ActiveKeyID: "old", Keys: map[string]ed25519.PublicKey{"old": oldPublic}, Audience: "energylens-cloud"}
	token, err := old.signVerified(uuid.NewString(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rotated := LocalAccessCodec{Private: newPrivate, ActiveKeyID: "new", Keys: map[string]ed25519.PublicKey{"old": oldPublic, "new": newPublic}, Audience: "energylens-cloud"}
	if _, _, err = rotated.verifySignature(token); err != nil {
		t.Fatalf("overlap rejected old key: %v", err)
	}
	publicOnly := rotated
	publicOnly.Private = nil
	if _, _, err = publicOnly.verifySignature(token); err != nil {
		t.Fatalf("public-only verifier rejected old key: %v", err)
	}
	retired := rotated
	retired.Keys = map[string]ed25519.PublicKey{"new": newPublic}
	if _, _, err = retired.verifySignature(token); err == nil {
		t.Fatal("retired key accepted")
	}
	rotated.ActiveKeyID = "old"
	if _, err = rotated.signVerified(uuid.NewString(), 1, time.Now()); err == nil {
		t.Fatal("mismatched active private key accepted")
	}
	if _, err = old.signVerified(uuid.NewString(), 0, time.Now()); err == nil {
		t.Fatal("missing auth version accepted")
	}
	missingKid := jwt.NewWithClaims(jwt.SigningMethodEdDSA, localAccessClaims{Version: 1, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: LocalIssuer, Subject: uuid.NewString(), Audience: jwt.ClaimStrings{"energylens-cloud"}, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(time.Now()), NotBefore: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(localAccessLifetime)),
	}})
	noKidToken, err := missingKid.SignedString(oldPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = old.verifySignature(noKidToken); err == nil {
		t.Fatal("valid signature without kid accepted")
	}
}
