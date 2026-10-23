package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResetUsernamePassword is the username profile's intentionally unauthenticated
// reset flow. Its public result is the same for unknown and throttled IDs.
func (s LocalLoginService) ResetUsernamePassword(ctx context.Context, username, password, source string, now time.Time) error {
	if !s.configured() || s.Codec.Profile != LocalUsernameProfile || !validLocalAuthSource(source) || now.IsZero() {
		return ErrInvalidCredential
	}
	normalized, err := NormalizeLocalUsername(username)
	if err != nil || !validPassword(password) {
		return ErrInvalidCredential
	}
	newHash, err := HashLocalPassword(password)
	if err != nil {
		return err
	}
	// Commit the attempt separately so collisions and failed writes retain their
	// budget. Reuse the existing recovery table's hourly/daily windows; the
	// username HMAC domains keep these keys separate from email recovery.
	rateTx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer rateTx.Rollback(ctx)
	allowedSource, err := recoveryRate(ctx, rateTx, "source_hour", s.rateHash("username-reset-source", source), now, time.Hour, 10)
	if err != nil {
		return err
	}
	allowedID, err := recoveryRate(ctx, rateTx, "email_day", s.rateHash("username-reset-id", normalized), now, 24*time.Hour, 3)
	if err != nil {
		return err
	}
	if err := rateTx.Commit(ctx); err != nil {
		return err
	}
	if !allowedSource || !allowedID {
		return nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		err = s.resetUsernamePasswordAttempt(ctx, normalized, newHash, now)
		if !errors.Is(err, errNewRefreshFamily) {
			return err
		}
	}
	return errors.New("username password reset contention")
}

func (s LocalLoginService) resetUsernamePasswordAttempt(ctx context.Context, normalized, newHash string, now time.Time) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `SELECT c.user_id FROM auth_password_credentials c
		JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.username_normalized=$1 AND c.credential_kind='username_local'`, normalized, LocalUsernameIssuer).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT family_id::text FROM auth_refresh_sessions WHERE user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return err
	}
	families := make(map[string]bool)
	var ordered []string
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			rows.Close()
			return err
		}
		families[family] = true
		ordered = append(ordered, family)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, family := range ordered {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, family); err != nil {
			return err
		}
	}
	var verified, activated, deleted *time.Time
	var profileVerified bool
	err = tx.QueryRow(ctx, `SELECT c.verified_at,c.activated_at,u.deleted_at,u.email_verified
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.user_id=$1 AND c.credential_kind='username_local' FOR UPDATE OF c,u`, userID, LocalUsernameIssuer).
		Scan(&verified, &activated, &deleted, &profileVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT family_id::text FROM auth_refresh_sessions WHERE user_id=$1`, userID)
	if err != nil {
		return err
	}
	newFamily := false
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			rows.Close()
			return err
		}
		if !families[family] {
			newFamily = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if newFamily {
		return errNewRefreshFamily
	}
	if deleted != nil || !LocalUsernameProfile.eligible(verified, activated, profileVerified) {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_credentials SET password_hash=$2,access_version=access_version+1,updated_at=$3 WHERE user_id=$1`, userID, newHash, now.UTC()); err != nil {
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
