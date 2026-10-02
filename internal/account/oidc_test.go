package account

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestOIDCVerifierRejectsInvalidIdentityTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "one", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer jwks.Close()
	v := &OIDCVerifier{Issuer: "https://issuer.example/", Audience: "energylens", JWKSURL: jwks.URL}
	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "one"
		out, e := tok.SignedString(key)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	now := time.Now()
	base := jwt.MapClaims{"iss": v.Issuer, "aud": v.Audience, "sub": "account-1", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "email": "person@example.test", "email_verified": true}
	id, err := v.Verify(context.Background(), sign(base))
	if err != nil || id.Issuer != v.Issuer || id.Subject != "account-1" || id.Email != "person@example.test" {
		t.Fatalf("valid token: %+v %v", id, err)
	}
	for name, change := range map[string]func(jwt.MapClaims){"issuer": func(c jwt.MapClaims) { c["iss"] = "https://other.example/" }, "audience": func(c jwt.MapClaims) { c["aud"] = "other" }, "expired": func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Hour).Unix() }, "no_subject": func(c jwt.MapClaims) { delete(c, "sub") }, "no_expiry": func(c jwt.MapClaims) { delete(c, "exp") }} {
		t.Run(name, func(t *testing.T) {
			c := jwt.MapClaims{}
			for k, val := range base {
				c[k] = val
			}
			change(c)
			if _, err := v.Verify(context.Background(), sign(c)); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	c := jwt.MapClaims{}
	for k, val := range base {
		c[k] = val
	}
	c["email_verified"] = false
	id, err = v.Verify(context.Background(), sign(c))
	if err != nil || id.Email != "" || id.EmailVerified {
		t.Fatalf("unverified email trusted: %+v %v", id, err)
	}
}
