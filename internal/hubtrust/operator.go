package hubtrust

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"
)

// IssueEnrollmentNonce is an operator-only action: its caller must connect with
// the dedicated migration/provisioning database credential, never the API role.
func (s Store) IssueEnrollmentNonce(ctx context.Context, hubID string) (string, error) {
	nonce, e := random32()
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(nonce)
	var exists string
	e = s.Pool.QueryRow(ctx, `SELECT id FROM hubs WHERE id=$1 AND revoked_at IS NULL`, hubID).Scan(&exists)
	if e != nil {
		return "", ErrNotFound
	}
	_, e = s.Pool.Exec(ctx, `INSERT INTO hub_enrollment_nonces(nonce_hash,hub_id,expires_at) VALUES($1,$2,$3)`, h[:], hubID, time.Now().Add(enrollTTL))
	if e != nil {
		return "", e
	}
	return b64(nonce), nil
}
func PublicKey(s string) (ed25519.PublicKey, error) {
	b, e := decode64(s, ed25519.PublicKeySize)
	if e != nil {
		return nil, errors.New("public key must be unpadded base64url Ed25519 key")
	}
	return ed25519.PublicKey(b), nil
}
