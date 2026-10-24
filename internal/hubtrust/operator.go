package hubtrust

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IssueEnrollmentNonce issues operator nonces and must use the operator
// (migration/provisioning) credential; recovery nonces are issued by the API.
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

// IssueRecoveryNonce lets an unclaimed hub prove its device key and receive a
// one-use enrollment nonce bound to the current ownership epoch.
func (s Store) IssueRecoveryNonce(ctx context.Context, hubID, local, signedAt string, sig []byte) (string, int64, time.Time, error) {
	var key []byte
	var storedLocal string
	var epoch int64
	var home *string
	var revoked bool
	const sel = `SELECT local_hub_id,device_public_key,ownership_epoch,home_id,revoked_at IS NOT NULL FROM hubs WHERE id=$1`
	e := s.Pool.QueryRow(ctx, sel, hubID).Scan(&storedLocal, &key, &epoch, &home, &revoked)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", 0, time.Time{}, ErrDenied
	}
	if e != nil {
		return "", 0, time.Time{}, e
	}
	ts, e := time.Parse(time.RFC3339, signedAt)
	if revoked || storedLocal != local || e != nil || time.Since(ts) > challengeMaxAge || time.Until(ts) > time.Minute {
		return "", 0, time.Time{}, ErrDenied
	}
	if !ed25519.Verify(key, enrollChallengeTranscript(hubID, local, signedAt), sig) {
		return "", 0, time.Time{}, ErrDenied
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return "", 0, time.Time{}, e
	}
	defer tx.Rollback(ctx)
	var curEpoch int64
	e = tx.QueryRow(ctx, `SELECT ownership_epoch,home_id,revoked_at IS NOT NULL FROM hubs WHERE id=$1 FOR UPDATE`, hubID).Scan(&curEpoch, &home, &revoked)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", 0, time.Time{}, ErrDenied
	}
	if e != nil {
		return "", 0, time.Time{}, e
	}
	if revoked || curEpoch != epoch {
		return "", 0, time.Time{}, ErrDenied
	}
	if home != nil {
		return "", 0, time.Time{}, ErrClaimed
	}
	if _, e = tx.Exec(ctx, `DELETE FROM hub_enrollment_nonces WHERE hub_id=$1 AND purpose=$2 AND consumed_at IS NULL AND expires_at<=now()`, hubID, recoveryPurpose); e != nil {
		return "", 0, time.Time{}, e
	}
	var open int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM hub_enrollment_nonces WHERE hub_id=$1 AND purpose=$2 AND consumed_at IS NULL`, hubID, recoveryPurpose).Scan(&open); e != nil {
		return "", 0, time.Time{}, e
	}
	if open >= recoveryMaxOutstanding {
		return "", 0, time.Time{}, ErrRateLimited
	}
	nonce, e := random32()
	if e != nil {
		return "", 0, time.Time{}, e
	}
	h := sha256.Sum256(nonce)
	expires := time.Now().UTC().Add(enrollTTL)
	if _, e = tx.Exec(ctx, `INSERT INTO hub_enrollment_nonces(nonce_hash,hub_id,expires_at,purpose,ownership_epoch) VALUES($1,$2,$3,$4,$5)`, h[:], hubID, expires, recoveryPurpose, epoch); e != nil {
		return "", 0, time.Time{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", 0, time.Time{}, e
	}
	return b64(nonce), epoch, expires, nil
}
