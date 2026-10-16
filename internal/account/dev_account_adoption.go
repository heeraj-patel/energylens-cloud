package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DevLocalIssuer identifies the development account whose existing home is retained.
const DevLocalIssuer = "urn:energylens:dev-local:v1"
const DevLocalUserID = "a225d5f0-22b7-4080-b5d6-e69ce7741b4c"

var ErrDevAccountAdoptionConflict = errors.New("development account adoption conflict")
var ErrDevLoginUnlockConflict = errors.New("development login unlock conflict")

// UnlockDevUsernameLogin removes only the exhausted account bucket for the
// approved local development identity. Source buckets and sessions remain intact.
func UnlockDevUsernameLogin(ctx context.Context, db *pgxpool.Pool, rateKey []byte, userID, username string) error {
	if db == nil || userID != DevLocalUserID || username != "dev" || len(rateKey) < 32 {
		return ErrDevLoginUnlockConflict
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var found string
	err = tx.QueryRow(ctx, `SELECT c.user_id FROM auth_password_credentials c
		JOIN users u ON u.id=c.user_id
		JOIN auth_identities d ON d.user_id=u.id AND d.issuer=$3 AND d.subject=u.email
		JOIN auth_identities l ON l.user_id=u.id AND l.issuer=$4 AND l.subject=u.id::text
		WHERE c.user_id=$1 AND c.username_normalized=$2 AND c.credential_kind='username_local'
		AND c.activated_at IS NOT NULL AND c.verified_at IS NULL
		AND u.email_verified=true AND u.deleted_at IS NULL
		FOR UPDATE OF c,u`, userID, username, DevLocalIssuer, LocalUsernameIssuer).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDevLoginUnlockConflict
	}
	if err != nil {
		return err
	}
	key := (LocalLoginService{RateKey: rateKey}).rateHash("account", "username_local:"+username)
	result, err := tx.Exec(ctx, `DELETE FROM auth_login_rate_limits
		WHERE scope='account_hour' AND key_hash=$1 AND attempts>=5`, key)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrDevLoginUnlockConflict
	}
	return tx.Commit(ctx)
}

// AdoptDevAccountUsername attaches a runtime-local credential to an existing
// verified development account. It never creates a user or changes a home.
func AdoptDevAccountUsername(ctx context.Context, db *pgxpool.Pool, userID, username, password string, now time.Time) error {
	if db == nil || !canonicalLocalUserID(userID) || now.IsZero() {
		return ErrInvalidCredential
	}
	normalized, err := NormalizeLocalUsername(username)
	if err != nil {
		return ErrInvalidCredential
	}
	if !validPassword(password) {
		return ErrInvalidCredential
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var email string
	err = tx.QueryRow(ctx, `SELECT u.email FROM users u
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.email
		WHERE u.id=$1 AND u.email IS NOT NULL AND u.email_verified=true AND u.deleted_at IS NULL
		FOR UPDATE OF u`, userID, DevLocalIssuer).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDevAccountAdoptionConflict
	}
	if err != nil {
		return err
	}
	var kind, existingHash string
	var existingUsername *string
	err = tx.QueryRow(ctx, `SELECT credential_kind,username_normalized,password_hash
		FROM auth_password_credentials WHERE user_id=$1 FOR UPDATE`, userID).Scan(&kind, &existingUsername, &existingHash)
	if err == nil {
		var identityUser string
		identityErr := tx.QueryRow(ctx, `SELECT user_id FROM auth_identities
			WHERE issuer=$1 AND subject=$2`, LocalUsernameIssuer, userID).Scan(&identityUser)
		if kind != "username_local" || existingUsername == nil || *existingUsername != normalized ||
			identityErr != nil || identityUser != userID || !VerifyLocalPassword(existingHash, password) {
			return ErrDevAccountAdoptionConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var existingIdentity string
	err = tx.QueryRow(ctx, `SELECT user_id FROM auth_identities
		WHERE issuer=$1 AND subject=$2`, LocalUsernameIssuer, userID).Scan(&existingIdentity)
	if err == nil {
		return ErrDevAccountAdoptionConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	hash, err := HashLocalPassword(password)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO auth_password_credentials
		(user_id,credential_kind,username_normalized,password_hash,activated_at)
		VALUES($1,'username_local',$2,$3,$4)`, userID, normalized, hash, now.UTC())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDevAccountAdoptionConflict
		}
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject)
		VALUES($1,$2,$1::uuid::text)`, userID, LocalUsernameIssuer)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDevAccountAdoptionConflict
		}
		return err
	}
	return tx.Commit(ctx)
}
