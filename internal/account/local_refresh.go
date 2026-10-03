package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LocalRefreshService is inactive until local HTTP auth, key configuration,
// and trusted verifier dispatch are installed together.
type LocalRefreshService struct {
	DB    *pgxpool.Pool
	Codec LocalAccessCodec
}

func (s LocalRefreshService) configured() bool { return s.DB != nil && s.Codec.valid() }

type refreshRow struct {
	ID, UserID, FamilyID string
	Expires              time.Time
	Revoked              *time.Time
	RotatedTo            *string
}

// lockRefreshFamily obtains one transaction-scoped lock before any session
// row lock. Two rotations or a replay within a family cannot race each other.
func lockRefreshFamily(ctx context.Context, tx pgx.Tx, hash []byte) (string, error) {
	var family string
	err := tx.QueryRow(ctx, `SELECT family_id FROM auth_refresh_sessions WHERE token_hash=$1`, hash).Scan(&family)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, family)
	return family, err
}

func readRefresh(ctx context.Context, tx pgx.Tx, hash []byte) (refreshRow, error) {
	var row refreshRow
	err := tx.QueryRow(ctx, `SELECT id,user_id,family_id,expires_at,revoked_at,rotated_to
		FROM auth_refresh_sessions WHERE token_hash=$1 FOR UPDATE`, hash).
		Scan(&row.ID, &row.UserID, &row.FamilyID, &row.Expires, &row.Revoked, &row.RotatedTo)
	return row, err
}

func revokeRefreshFamily(ctx context.Context, tx pgx.Tx, family string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE auth_refresh_sessions SET revoked_at=$2
		WHERE family_id=$1 AND revoked_at IS NULL`, family, now.UTC())
	return err
}

// Rotate consumes exactly one current refresh token and mints one successor.
// Reuse of a rotated token revokes every session in its family. The family
// expires at most 30 days after its first session, even through rotations.
func (s LocalRefreshService) Rotate(ctx context.Context, raw string, now time.Time) (LocalLoginTokens, error) {
	if !s.configured() || now.IsZero() {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	hash, err := HashLocalOpaqueToken("refresh", raw)
	if err != nil {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	defer tx.Rollback(ctx)
	family, err := lockRefreshFamily(ctx, tx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	if err != nil {
		return LocalLoginTokens{}, err
	}
	row, err := readRefresh(ctx, tx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	if err != nil {
		return LocalLoginTokens{}, err
	}
	if row.FamilyID != family {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	if row.RotatedTo != nil {
		if err := revokeRefreshFamily(ctx, tx, family, now); err != nil {
			return LocalLoginTokens{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LocalLoginTokens{}, err
		}
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	if row.Revoked != nil || !now.Before(row.Expires) {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	var deleted, verified *time.Time
	var profileVerified bool
	var accessVersion int64
	err = tx.QueryRow(ctx, `SELECT u.deleted_at,c.verified_at,u.email_verified,c.access_version
		FROM users u JOIN auth_password_credentials c ON c.user_id=u.id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE u.id=$1 FOR UPDATE OF u,c`, row.UserID, LocalIssuer).Scan(&deleted, &verified, &profileVerified, &accessVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	if err != nil {
		return LocalLoginTokens{}, err
	}
	if deleted != nil || verified == nil || !profileVerified {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	var firstCreated time.Time
	if err = tx.QueryRow(ctx, `SELECT min(created_at) FROM auth_refresh_sessions WHERE family_id=$1`, family).Scan(&firstCreated); err != nil {
		return LocalLoginTokens{}, err
	}
	absoluteExpiry := firstCreated.Add(localRefreshLifetime)
	if !now.Before(absoluteExpiry) {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	// A refresh rotates credentials but does not establish a new sign-in.
	access, err := s.Codec.signVerifiedAt(row.UserID, accessVersion, now, firstCreated)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	newRaw, newHash, err := NewLocalOpaqueToken("refresh")
	if err != nil {
		return LocalLoginTokens{}, err
	}
	child := uuid.NewString()
	if _, err = tx.Exec(ctx, `INSERT INTO auth_refresh_sessions(id,user_id,family_id,token_hash,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6)`, child, row.UserID, family, newHash[:], now.UTC(), absoluteExpiry); err != nil {
		return LocalLoginTokens{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_refresh_sessions SET revoked_at=$2,rotated_to=$3 WHERE id=$1`, row.ID, now.UTC(), child); err != nil {
		return LocalLoginTokens{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LocalLoginTokens{}, err
	}
	return LocalLoginTokens{AccessToken: access, AccessExpiresAt: now.UTC().Add(localAccessLifetime), RefreshToken: newRaw, RefreshExpiresAt: absoluteExpiry}, nil
}

// Logout is idempotent and revokes the whole family for any known token.
func (s LocalRefreshService) Logout(ctx context.Context, raw string, now time.Time) error {
	if !s.configured() || now.IsZero() {
		return ErrInvalidCredential
	}
	hash, err := HashLocalOpaqueToken("refresh", raw)
	if err != nil {
		return nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	family, err := lockRefreshFamily(ctx, tx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = revokeRefreshFamily(ctx, tx, family, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
