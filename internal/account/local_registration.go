package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VerificationSender is not installed in the runtime. A future adapter must
// deliver only the token supplied here and must not log it or its recipient.
type VerificationSender interface {
	SendVerification(context.Context, string, string) error
}

// RegistrationAccepted is identical for a new address, an existing address,
// a deleted account, and a throttled request. No route exposes this yet.
type RegistrationAccepted struct {
	Status string `json:"status"`
}

type LocalRegistrationService struct {
	DB      *pgxpool.Pool
	Sender  VerificationSender
	RateKey []byte // independently provisioned 32+ byte HMAC key
	Profile LocalAuthProfile
}

func (s LocalRegistrationService) configured() bool {
	return s.DB != nil && len(s.RateKey) >= 32 && s.Profile.valid() && (s.Profile == LocalUsernameProfile || s.Sender != nil)
}

func (s LocalRegistrationService) rateHash(scope, value string) []byte {
	h := hmac.New(sha256.New, s.RateKey)
	h.Write([]byte("energylens.local-registration." + scope + ".v1\n"))
	h.Write([]byte(value))
	return h.Sum(nil)
}

func registrationRate(ctx context.Context, tx pgx.Tx, scope string, key []byte, now time.Time, window time.Duration, limit int) (bool, error) {
	var attempts int
	err := tx.QueryRow(ctx, `INSERT INTO auth_registration_rate_limits(scope,key_hash,window_start,attempts)
		VALUES($1,$2,$3,1)
		ON CONFLICT(scope,key_hash) DO UPDATE SET
		  attempts=CASE WHEN auth_registration_rate_limits.window_start<=$4 THEN 1 ELSE LEAST(auth_registration_rate_limits.attempts+1,1000000) END,
		  window_start=CASE WHEN auth_registration_rate_limits.window_start<=$4 THEN $3 ELSE auth_registration_rate_limits.window_start END
		RETURNING attempts`, scope, key, now, now.Add(-window)).Scan(&attempts)
	return attempts <= limit, err
}

// RequestRegistration records a private verification challenge and sends it
// only after commit. Every valid request returns the same public outcome. The
// source must be the trusted client address selected by the future HTTP edge.
func (s LocalRegistrationService) RequestRegistration(ctx context.Context, email, password, source string, now time.Time) (RegistrationAccepted, error) {
	accepted := RegistrationAccepted{Status: "accepted"}
	if !s.configured() || s.Profile == LocalUsernameProfile {
		return RegistrationAccepted{}, errors.New("local registration is not configured")
	}
	normalized, err := NormalizeLocalEmail(email)
	if err != nil || !validPassword(password) || !validLocalAuthSource(source) || now.IsZero() {
		return RegistrationAccepted{}, ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	defer tx.Rollback(ctx)
	allowed, err := registrationRate(ctx, tx, "source_hour", s.rateHash("source", source), now, time.Hour, 10)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		return accepted, tx.Commit(ctx)
	}
	allowed, err = registrationRate(ctx, tx, "email_day", s.rateHash("email", normalized), now, 24*time.Hour, 3)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		return accepted, tx.Commit(ctx)
	}
	// Hash on both new and existing valid-address paths so the work is similar.
	hash, err := HashLocalPassword(password)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	var userID string
	var verifiedAt, deletedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT c.user_id,c.verified_at,u.deleted_at
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		WHERE c.email_normalized=$1 FOR UPDATE OF c,u`, normalized).Scan(&userID, &verifiedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		userID = uuid.NewString()
		if _, err = tx.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
			return RegistrationAccepted{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,email_normalized,password_hash) VALUES($1,$2,$3)`, userID, normalized, hash); err != nil {
			return RegistrationAccepted{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, userID, LocalIssuer, userID); err != nil {
			return RegistrationAccepted{}, err
		}
	} else if err != nil {
		return RegistrationAccepted{}, err
	} else if verifiedAt != nil || deletedAt != nil {
		return accepted, tx.Commit(ctx)
	}
	raw, tokenHash, err := NewLocalOpaqueToken("verify_email")
	if err != nil {
		return RegistrationAccepted{}, err
	}
	// A resend invalidates earlier challenges. The row lock above serializes
	// resends for one credential, and token identity never changes.
	if _, err = tx.Exec(ctx, `UPDATE auth_email_verification_tokens SET consumed_at=$2
		WHERE user_id=$1 AND consumed_at IS NULL`, userID, now); err != nil {
		return RegistrationAccepted{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth_email_verification_tokens(token_hash,user_id,created_at,expires_at)
		VALUES($1,$2,$3,$4)`, tokenHash[:], userID, now, now.Add(time.Hour)); err != nil {
		return RegistrationAccepted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RegistrationAccepted{}, err
	}
	// Delivery failure has the same public outcome; a bounded retry can issue a
	// new challenge. Never return sender errors, which may contain private data.
	_ = s.Sender.SendVerification(ctx, normalized, raw)
	return accepted, nil
}

// VerifyEmail consumes a single challenge and marks the same active user as
// verified. The user row lock serializes this with account deletion.
func (s LocalRegistrationService) VerifyEmail(ctx context.Context, raw string, now time.Time) error {
	if !s.configured() || s.Profile == LocalUsernameProfile || now.IsZero() {
		return ErrInvalidCredential
	}
	tokenHash, err := HashLocalOpaqueToken("verify_email", raw)
	if err != nil {
		return ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID, email string
	var expires time.Time
	var consumed, verified, deleted *time.Time
	// Read the immutable token owner, then lock credential/user before token.
	// Registration resend takes those locks in the same order.
	err = tx.QueryRow(ctx, `SELECT user_id FROM auth_email_verification_tokens WHERE token_hash=$1`, tokenHash[:]).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT c.email_normalized,c.verified_at,u.deleted_at
		FROM auth_password_credentials c JOIN users u ON u.id=c.user_id
		WHERE c.user_id=$1 FOR UPDATE OF c,u`, userID).Scan(&email, &verified, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT expires_at,consumed_at FROM auth_email_verification_tokens
		WHERE token_hash=$1 AND user_id=$2 FOR UPDATE`, tokenHash[:], userID).Scan(&expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredential
	}
	if err != nil {
		return err
	}
	if consumed != nil || verified != nil || deleted != nil || !now.Before(expires) {
		return ErrInvalidCredential
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_password_credentials SET verified_at=$2,updated_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET email=$2,email_verified=true,updated_at=$3 WHERE id=$1 AND deleted_at IS NULL`, userID, email, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_email_verification_tokens SET consumed_at=$2 WHERE user_id=$1 AND consumed_at IS NULL`, userID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var ErrUsernameInUse = errors.New("username in use")
var ErrRegistrationRateLimited = errors.New("registration rate limited")

// RegisterUsername uses the same account and identity tables as email signup.
// The unique index makes concurrent registration atomic; a failed insert rolls
// back the user row and never creates a partial identity or credential.
func (s LocalRegistrationService) RegisterUsername(ctx context.Context, username, password, source string, now time.Time) (RegistrationAccepted, error) {
	if !s.configured() || s.Profile != LocalUsernameProfile {
		return RegistrationAccepted{}, errors.New("local username registration is not configured")
	}
	normalized, err := NormalizeLocalUsername(username)
	if err != nil || !validPassword(password) || !validLocalAuthSource(source) || now.IsZero() {
		return RegistrationAccepted{}, ErrInvalidCredential
	}
	// Commit the rate attempt before password hashing or account insertion. A
	// collision and an aborted account transaction still consume the budget.
	rateTx, err := s.DB.Begin(ctx)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	defer rateTx.Rollback(ctx)
	allowed, err := registrationRate(ctx, rateTx, "source_hour", s.rateHash("source", source), now, time.Hour, 10)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		if err := rateTx.Commit(ctx); err != nil {
			return RegistrationAccepted{}, err
		}
		return RegistrationAccepted{}, ErrRegistrationRateLimited
	}
	allowed, err = registrationRate(ctx, rateTx, "username_day", s.rateHash("username", normalized), now, 24*time.Hour, 10)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	if err := rateTx.Commit(ctx); err != nil {
		return RegistrationAccepted{}, err
	}
	if !allowed {
		return RegistrationAccepted{}, ErrRegistrationRateLimited
	}
	hash, err := HashLocalPassword(password)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RegistrationAccepted{}, err
	}
	defer tx.Rollback(ctx)
	userID := uuid.NewString()
	if _, err = tx.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		return RegistrationAccepted{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,credential_kind,username_normalized,password_hash,activated_at)
		VALUES($1,'username_local',$2,$3,$4)`, userID, normalized, hash, now.UTC()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "auth_password_credentials_username_local_unique" {
			return RegistrationAccepted{}, ErrUsernameInUse
		}
		return RegistrationAccepted{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, userID, LocalUsernameIssuer, userID); err != nil {
		return RegistrationAccepted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RegistrationAccepted{}, err
	}
	return RegistrationAccepted{Status: "account_created"}, nil
}
