package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type dispatchVerifier struct {
	identity Identity
	err      error
	calls    *int
}

func (v dispatchVerifier) Verify(_ context.Context, _ string) (Identity, error) {
	*v.calls++
	return v.identity, v.err
}

func TestDualVerifierSelectsOneIssuerWithoutFallback(t *testing.T) {
	ctx := context.Background()
	oidcCalls, localCalls := 0, 0
	v := DualVerifier{OIDCIssuer: "https://issuer.example", OIDC: dispatchVerifier{identity: Identity{Issuer: "https://issuer.example", Subject: "oidc"}, calls: &oidcCalls}, Local: dispatchVerifier{identity: Identity{Issuer: LocalIssuer, Subject: "local"}, calls: &localCalls}}
	sign := func(issuer string) string {
		t.Helper()
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Issuer: issuer, Subject: "subject", IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))})
		raw, err := token.SignedString([]byte("test-only"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if id, err := v.Verify(ctx, sign(v.OIDCIssuer)); err != nil || id.Subject != "oidc" || oidcCalls != 1 || localCalls != 0 {
		t.Fatalf("OIDC dispatch: %+v %v %d %d", id, err, oidcCalls, localCalls)
	}
	if id, err := v.Verify(ctx, sign(LocalIssuer)); err != nil || id.Subject != "local" || oidcCalls != 1 || localCalls != 1 {
		t.Fatalf("local dispatch: %+v %v %d %d", id, err, oidcCalls, localCalls)
	}
	if _, err := v.Verify(ctx, sign("https://unknown.example")); err == nil || oidcCalls != 1 || localCalls != 1 {
		t.Fatal("unknown issuer reached verifier")
	}
	v.Local = dispatchVerifier{err: errors.New("invalid local signature"), calls: &localCalls}
	if _, err := v.Verify(ctx, sign(LocalIssuer)); err == nil || oidcCalls != 1 || localCalls != 2 {
		t.Fatal("failed local verifier fell back to OIDC")
	}
	v.Local = dispatchVerifier{identity: Identity{Issuer: v.OIDCIssuer, Subject: "wrong"}, calls: &localCalls}
	if _, err := v.Verify(ctx, sign(LocalIssuer)); err == nil {
		t.Fatal("verifier identity mismatch accepted")
	}
}
