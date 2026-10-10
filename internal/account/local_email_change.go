package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrEmailInUse = errors.New("email address already in use")

// ChangeEmail updates the verified local login address after a current-password
// check. Profile versioning and replay records make retries safe for the demo UI.
func (s LocalLoginService) ChangeEmail(ctx context.Context, userID, email, currentPassword, source string, version int64, key string, requestHash []byte, now time.Time) (User, error) {
	if !s.configured() || !canonicalLocalUserID(userID) || !validLocalAuthSource(source) || version < 1 || !validUUID(key) || len(requestHash) != sha256.Size || now.IsZero() {
		return User{}, ErrInvalidCredential
	}
	normalized, err := NormalizeLocalEmail(email)
	if err != nil || normalized != email {
		return User{}, ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)
	allowed, err := loginRate(ctx, tx, "source_hour", s.rateHash("email-change-source", source), now, 20)
	if err != nil {
		return User{}, err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return User{}, err
		}
		return User{}, ErrInvalidCredential
	}
	allowed, err = loginRate(ctx, tx, "account_hour", s.rateHash("email-change-account", userID), now, 5)
	if err != nil {
		return User{}, err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return User{}, err
		}
		return User{}, ErrInvalidCredential
	}
	var stored string
	var verified, deleted *time.Time
	var userVerified bool
	err = tx.QueryRow(ctx, `SELECT c.password_hash,c.verified_at,u.deleted_at,u.email_verified
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.user_id=$1 FOR UPDATE OF c,u`, userID, LocalIssuer).Scan(&stored, &verified, &deleted, &userVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	var replay User
	if found, replayErr := prefReplay(ctx, tx, userID, "local_email_change", key, requestHash, &replay); found || replayErr != nil {
		return replay, replayErr
	}
	var currentVersion int64
	if err = tx.QueryRow(ctx, `SELECT profile_version FROM users WHERE id=$1`, userID).Scan(&currentVersion); err != nil {
		return User{}, err
	}
	if currentVersion != version {
		return User{}, ErrVersion
	}
	if verified == nil || deleted != nil || !userVerified || !VerifyLocalPassword(stored, currentPassword) {
		if err := tx.Commit(ctx); err != nil {
			return User{}, err
		}
		return User{}, ErrInvalidCredential
	}
	var existingEmail *string
	if err = tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&existingEmail); err != nil {
		return User{}, err
	}
	if existingEmail != nil && strings.EqualFold(*existingEmail, normalized) {
		// Same address is an idempotent no-op, but still record the request result.
		var current User
		if err = tx.QueryRow(ctx, `SELECT id,email,email_verified,display_name,profile_version,created_at FROM users WHERE id=$1`, userID).
			Scan(&current.ID, &current.Email, &current.EmailVerified, &current.DisplayName, &current.ProfileVersion, &current.CreatedAt); err != nil {
			return User{}, err
		}
		if err = savePrefReplay(ctx, tx, userID, "local_email_change", key, requestHash, current); err != nil {
			return User{}, err
		}
		return current, tx.Commit(ctx)
	}
	var updated User
	err = tx.QueryRow(ctx, `UPDATE users SET email=$2,email_verified=true,profile_version=profile_version+1,updated_at=$3
		WHERE id=$1 AND deleted_at IS NULL
		RETURNING id,email,email_verified,display_name,profile_version,created_at`, userID, normalized, now.UTC()).
		Scan(&updated.ID, &updated.Email, &updated.EmailVerified, &updated.DisplayName, &updated.ProfileVersion, &updated.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, ErrEmailInUse
		}
		return User{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_credentials SET email_normalized=$2,updated_at=$3 WHERE user_id=$1`, userID, normalized, now.UTC()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, ErrEmailInUse
		}
		return User{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_email_verification_tokens SET consumed_at=$2 WHERE user_id=$1 AND consumed_at IS NULL`, userID, now.UTC()); err != nil {
		return User{}, err
	}
	if err = savePrefReplay(ctx, tx, userID, "local_email_change", key, requestHash, updated); err != nil {
		return User{}, err
	}
	return updated, tx.Commit(ctx)
}

func (h LocalAuthHTTP) changeEmail(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	version, ok := matchVersion(w, r)
	if !ok {
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewEmail        string `json:"new_email"`
	}
	_, ok = decode(w, r, &in)
	if !ok {
		return
	}
	if !validPassword(in.CurrentPassword) {
		localAuthError(w, ErrInvalidCredential)
		return
	}
	normalized, err := NormalizeLocalEmail(in.NewEmail)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid email address")
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
	hash := sha256.Sum256([]byte(strconv.FormatInt(version, 10) + "\n" + normalized))
	updated, err := h.Login.ChangeEmail(r.Context(), identity.Subject, normalized, in.CurrentPassword, source, version, key, hash[:], time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, ErrVersion):
			fail(w, http.StatusConflict, "version_conflict", "profile version changed")
		case errors.Is(err, ErrKeyReuse):
			fail(w, http.StatusConflict, "idempotency_key_reuse", "key was used with another request")
		case errors.Is(err, ErrInvalidCredential):
			localAuthError(w, err)
		case errors.Is(err, ErrEmailInUse):
			localAuthError(w, err)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrDeleted):
			fail(w, http.StatusForbidden, "account_deleted", "account is closed")
		default:
			internal(w, err)
		}
		return
	}
	etag(w, updated.ProfileVersion)
	jsonOut(w, http.StatusOK, updated)
}
