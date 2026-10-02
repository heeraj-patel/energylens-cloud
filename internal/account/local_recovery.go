package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RecoverySender is intentionally not installed in the runtime.
type RecoverySender interface {
	SendRecovery(context.Context, string, string) error
}

type LocalRecoveryService struct {
	DB      *pgxpool.Pool
	Sender  RecoverySender
	RateKey []byte // independent, provisioned HMAC key
}

func (s LocalRecoveryService) configured() bool {
	return s.DB != nil && s.Sender != nil && len(s.RateKey) >= 32
}

func (s LocalRecoveryService) rateHash(scope, value string) []byte {
	h := hmac.New(sha256.New, s.RateKey)
	h.Write([]byte("energylens.local-recovery." + scope + ".v1\n"))
	h.Write([]byte(value))
	return h.Sum(nil)
}

func recoveryRate(ctx context.Context, tx pgx.Tx, scope string, key []byte, now time.Time, window time.Duration, limit int) (bool, error) {
	var attempts int
	err := tx.QueryRow(ctx, `INSERT INTO auth_password_reset_rate_limits(scope,key_hash,window_start,attempts)
		VALUES($1,$2,$3,1)
		ON CONFLICT(scope,key_hash) DO UPDATE SET
		 attempts=CASE WHEN auth_password_reset_rate_limits.window_start<=$4 THEN 1 ELSE LEAST(auth_password_reset_rate_limits.attempts+1,1000000) END,
		 window_start=CASE WHEN auth_password_reset_rate_limits.window_start<=$4 THEN $3 ELSE auth_password_reset_rate_limits.window_start END
		RETURNING attempts`, scope, key, now.UTC(), now.Add(-window).UTC()).Scan(&attempts)
	return attempts <= limit, err
}

// RequestReset has the same public outcome for known, unknown, unverified,
// deleted and throttled accounts. The source must come from a trusted edge.
func (s LocalRecoveryService) RequestReset(ctx context.Context, email, source string, now time.Time) (RegistrationAccepted, error) {
	accepted := RegistrationAccepted{Status: "accepted"}
	if !s.configured() {
		return RegistrationAccepted{}, errors.New("local recovery is not configured")
	}
	normalized, err := NormalizeLocalEmail(email)
	if err != nil || !validLocalAuthSource(source) || now.IsZero() {
		return RegistrationAccepted{}, ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	defer tx.Rollback(ctx)
	allowed, err := recoveryRate(ctx, tx, "source_hour", s.rateHash("source", source), now, time.Hour, 10)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		return accepted, tx.Commit(ctx)
	}
	allowed, err = recoveryRate(ctx, tx, "email_day", s.rateHash("email", normalized), now, 24*time.Hour, 3)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		return accepted, tx.Commit(ctx)
	}
	var userID string
	var verified, deleted *time.Time
	var profileVerified bool
	err = tx.QueryRow(ctx, `SELECT c.user_id,c.verified_at,u.deleted_at,u.email_verified
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.email_normalized=$1 FOR UPDATE OF c,u`, normalized, LocalIssuer).
		Scan(&userID, &verified, &deleted, &profileVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return accepted, tx.Commit(ctx)
	}
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if verified == nil || deleted != nil || !profileVerified {
		return accepted, tx.Commit(ctx)
	}
	raw, hash, err := NewLocalOpaqueToken("reset_password")
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_reset_tokens SET consumed_at=$2 WHERE user_id=$1 AND consumed_at IS NULL`, userID, now.UTC()); err != nil {
		return RegistrationAccepted{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth_password_reset_tokens(token_hash,user_id,created_at,expires_at) VALUES($1,$2,$3,$4)`, hash[:], userID, now.UTC(), now.Add(time.Hour).UTC()); err != nil {
		return RegistrationAccepted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RegistrationAccepted{}, err
	}
	_ = s.Sender.SendRecovery(ctx, normalized, raw)
	return accepted, nil
}

// ResetPassword consumes one challenge and revokes all existing refresh
// sessions in the same transaction as the credential update.
func (s LocalRecoveryService) ResetPassword(ctx context.Context, raw, password string, now time.Time) error {
	if !s.configured() || now.IsZero() || !validPassword(password) {
		return ErrInvalidCredential
	}
	hash, err := HashLocalOpaqueToken("reset_password", raw)
	if err != nil {
		return ErrInvalidCredential
	}
	newHash, err := HashLocalPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id FROM auth_password_reset_tokens WHERE token_hash=$1`, hash[:]).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	// Refresh rotation locks family, then session, then user. Acquire every
	// existing family advisory lock first to preserve that lock order.
	rows, err := tx.Query(ctx, `SELECT DISTINCT family_id::text FROM auth_refresh_sessions WHERE user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return err
	}
	var families []string
	for rows.Next() {
		var family string
		if err = rows.Scan(&family); err != nil {
			rows.Close()
			return err
		}
		families = append(families, family)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, family := range families {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, family); err != nil {
			return err
		}
	}
	var verified, deleted *time.Time
	var profileVerified bool
	err = tx.QueryRow(ctx, `SELECT c.verified_at,u.deleted_at,u.email_verified FROM auth_password_credentials c
		JOIN users u ON u.id=c.user_id JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.user_id=$1 FOR UPDATE OF c,u`, userID, LocalIssuer).Scan(&verified, &deleted, &profileVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	var expires time.Time
	var consumed *time.Time
	err = tx.QueryRow(ctx, `SELECT expires_at,consumed_at FROM auth_password_reset_tokens WHERE token_hash=$1 AND user_id=$2 FOR UPDATE`, hash[:], userID).Scan(&expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	if verified == nil || deleted != nil || !profileVerified || consumed != nil || !now.Before(expires) {
		return ErrInvalidCredential
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_credentials SET password_hash=$2,updated_at=$3 WHERE user_id=$1`, userID, newHash, now.UTC()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_reset_tokens SET consumed_at=$2 WHERE user_id=$1 AND consumed_at IS NULL`, userID, now.UTC()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_refresh_sessions SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
