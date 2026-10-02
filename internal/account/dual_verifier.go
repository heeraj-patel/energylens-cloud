package account

import (
	"context"
	"errors"

	"github.com/golang-jwt/jwt/v5"
)

// The untrusted issuer selects exactly one fully validating verifier; a failed
// selected verifier never falls back to the other trust domain.
type DualVerifier struct {
	OIDCIssuer string
	OIDC       Verifier
	Local      Verifier
}

func (v DualVerifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if v.OIDCIssuer == "" || v.OIDCIssuer == LocalIssuer || v.OIDC == nil || v.Local == nil || len(raw) > 4096 {
		return Identity{}, ErrInvalidCredential
	}
	claims := &jwt.RegisteredClaims{}
	if _, _, err := new(jwt.Parser).ParseUnverified(raw, claims); err != nil {
		return Identity{}, ErrInvalidCredential
	}
	var selected Verifier
	switch claims.Issuer {
	case v.OIDCIssuer:
		selected = v.OIDC
	case LocalIssuer:
		selected = v.Local
	default:
		return Identity{}, ErrInvalidCredential
	}
	id, err := selected.Verify(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if id.Issuer != claims.Issuer || id.Subject == "" {
		return Identity{}, errors.New("bearer verifier returned mismatched identity")
	}
	return id, nil
}
