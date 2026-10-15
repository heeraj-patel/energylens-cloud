package account

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Identity struct {
	Issuer, Subject, Email string
	SessionID              string
	EmailVerified          bool
	AuthTime               time.Time
}
type Verifier interface {
	Verify(context.Context, string) (Identity, error)
}

// OIDCVerifier accepts only RS256 tokens from one configured issuer and
// audience. Key IDs are resolved from a configured JWKS endpoint, never from
// token-supplied jku/x5u URLs.
type OIDCVerifier struct {
	Issuer, Audience, JWKSURL string
	Client                    *http.Client
	mu                        sync.Mutex
	keys                      map[string]*rsa.PublicKey
	fetched                   time.Time
}

func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if v.Issuer == "" || v.Audience == "" || v.JWKSURL == "" {
		return Identity{}, errors.New("OIDC verifier is not configured")
	}
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("missing key ID")
		}
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.Issuer), jwt.WithAudience(v.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid {
		return Identity{}, errors.New("invalid bearer token")
	}
	c, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return Identity{}, errors.New("invalid token claims")
	}
	sub, _ := c["sub"].(string)
	if strings.TrimSpace(sub) == "" {
		return Identity{}, errors.New("missing subject")
	}
	email, _ := c["email"].(string)
	verified, _ := c["email_verified"].(bool)
	var authTime time.Time
	if seconds, ok := c["auth_time"].(float64); ok && seconds > 0 && seconds == float64(int64(seconds)) {
		authTime = time.Unix(int64(seconds), 0).UTC()
	}
	if !verified {
		email = ""
	}
	return Identity{Issuer: v.Issuer, Subject: sub, Email: email, EmailVerified: verified, AuthTime: authTime}, nil
}

func (v *OIDCVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.fetched) < 5*time.Minute {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("JWKS status %d", resp.StatusCode)
	}
	// Explicit tags keep the accepted JWK fields unambiguous.
	var rawDoc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("JWKS document is too large or unreadable")
	}
	if err := json.Unmarshal(body, &rawDoc); err != nil {
		return nil, err
	}
	next := map[string]*rsa.PublicKey{}
	for _, k := range rawDoc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		nb, e1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil || len(nb) < 256 || len(eb) == 0 || len(eb) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(eb).Int64()
		if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
			continue
		}
		next[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exponent)}
	}
	v.keys = next
	v.fetched = time.Now()
	key := next[kid]
	if key == nil {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}
