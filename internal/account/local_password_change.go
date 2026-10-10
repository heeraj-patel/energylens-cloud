package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChangePassword requires a verified local access token. The access version,
// refresh sessions and outstanding recovery challenges change atomically.
func (s LocalLoginService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword, source string, now time.Time) error {
	if !s.configured() || !canonicalLocalUserID(userID) || !validLocalAuthSource(source) || now.IsZero() {
		return ErrInvalidCredential
	}
	// A newly minted login family can appear between the family scan and the
	// credential lock. Retry if that happens, preserving refresh's lock order.
	for attempt := 0; attempt < 5; attempt++ {
		err := s.changePasswordAttempt(ctx, userID, currentPassword, newPassword, source, now)
		if !errors.Is(err, errNewRefreshFamily) {
			return err
		}
	}
	return errors.New("local password change contention")
}

var errNewRefreshFamily = errors.New("new refresh family")

func (s LocalLoginService) changePasswordAttempt(ctx context.Context, userID, currentPassword, newPassword, source string, now time.Time) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	allowed, err := loginRate(ctx, tx, "source_hour", s.rateHash("password-change-source", source), now, 20)
	if err != nil {
		return err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrInvalidCredential
	}
	allowed, err = loginRate(ctx, tx, "account_hour", s.rateHash("password-change-account", userID), now, 5)
	if err != nil {
		return err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrInvalidCredential
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
	var stored string
	var verified, deleted *time.Time
	var profileVerified bool
	err = tx.QueryRow(ctx, `SELECT c.password_hash,c.verified_at,u.deleted_at,u.email_verified
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.user_id=$1 FOR UPDATE OF c,u`, userID, LocalIssuer).
		Scan(&stored, &verified, &deleted, &profileVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	// A login takes the credential lock before inserting its family. Once we
	// hold that lock, this scan is stable until commit.
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
	if verified == nil || deleted != nil || !profileVerified || !VerifyLocalPassword(stored, currentPassword) || !validPassword(newPassword) {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrInvalidCredential
	}
	newHash, err := HashLocalPassword(newPassword)
	if err != nil {
		return err
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

func (h LocalAuthHTTP) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") || strings.TrimSpace(a[7:]) == "" {
		fail(w, http.StatusUnauthorized, "unauthorized", "bearer token required")
		return
	}
	identity, err := (LocalAccessVerifier{DB: h.Login.DB, Codec: h.Login.Codec}).Verify(r.Context(), strings.TrimSpace(a[7:]))
	if err != nil {
		fail(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
		return
	}
	source, err := h.SourcePolicy.Source(r)
	if err != nil {
		localAuthError(w, err)
		return
	}
	if err := h.Login.ChangePassword(r.Context(), identity.Subject, in.CurrentPassword, in.NewPassword, source, time.Now().UTC()); err != nil {
		localAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
