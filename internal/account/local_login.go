package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

const localRefreshLifetime = 30 * 24 * time.Hour

var dummyHashOnce sync.Once
var dummyHash string

func unknownAccountHash() string {
	dummyHashOnce.Do(func() {
		salt := []byte("local-auth-dummy")
		digest := argon2.IDKey([]byte("unusable local account password"), salt, passwordIterations, passwordMemoryKiB, passwordThreads, 32)
		dummyHash = fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemoryKiB, passwordIterations, passwordThreads,
			base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
	})
	return dummyHash
}

type LocalLoginTokens struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

type LocalLoginService struct {
	DB      *pgxpool.Pool
	Codec   LocalAccessCodec
	RateKey []byte // independently provisioned 32+ byte HMAC key
}

func (s LocalLoginService) configured() bool {
	return s.DB != nil && s.Codec.valid() && len(s.RateKey) >= 32
}

func (s LocalLoginService) rateHash(scope, value string) []byte {
	h := hmac.New(sha256.New, s.RateKey)
	h.Write([]byte("energylens.local-login." + scope + ".v1\n"))
	h.Write([]byte(value))
	return h.Sum(nil)
}

func loginRate(ctx context.Context, tx pgx.Tx, scope string, key []byte, now time.Time, limit int) (bool, error) {
	var attempts int
	err := tx.QueryRow(ctx, `INSERT INTO auth_login_rate_limits(scope,key_hash,window_start,attempts)
		VALUES($1,$2,$3,1)
		ON CONFLICT(scope,key_hash) DO UPDATE SET
		  attempts=CASE WHEN auth_login_rate_limits.window_start<=$4 THEN 1 ELSE LEAST(auth_login_rate_limits.attempts+1,1000000) END,
		  window_start=CASE WHEN auth_login_rate_limits.window_start<=$4 THEN $3 ELSE auth_login_rate_limits.window_start END
		RETURNING attempts`, scope, key, now, now.Add(-time.Hour)).Scan(&attempts)
	return attempts <= limit, err
}

// Login is not mounted on an HTTP route. Every denied attempt yields the same
// error, including unknown, wrong-password, unverified, deleted and throttled
// accounts. The future HTTP edge must supply a trusted client source.
func (s LocalLoginService) Login(ctx context.Context, email, password, source string, now time.Time) (LocalLoginTokens, error) {
	if !s.configured() {
		return LocalLoginTokens{}, errors.New("local login is not configured")
	}
	normalized, err := NormalizeLocalEmail(email)
	if err != nil || !validLocalAuthSource(source) || now.IsZero() {
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	defer tx.Rollback(ctx)
	allowed, err := loginRate(ctx, tx, "source_hour", s.rateHash("source", source), now, 20)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return LocalLoginTokens{}, err
		}
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	allowed, err = loginRate(ctx, tx, "account_hour", s.rateHash("account", normalized), now, 5)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	if !allowed {
		if err := tx.Commit(ctx); err != nil {
			return LocalLoginTokens{}, err
		}
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	var userID, stored string
	var accessVersion int64
	var credentialVerified, deleted *time.Time
	var profileVerified bool
	err = tx.QueryRow(ctx, `SELECT c.user_id,c.password_hash,c.verified_at,u.email_verified,u.deleted_at,c.access_version
		FROM auth_password_credentials c
		JOIN users u ON u.id=c.user_id
		JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.email_normalized=$1 FOR UPDATE OF c,u`, normalized, LocalIssuer).Scan(&userID, &stored, &credentialVerified, &profileVerified, &deleted, &accessVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		stored = unknownAccountHash()
	} else if err != nil {
		return LocalLoginTokens{}, err
	}
	// Always execute the bounded Argon2id comparison for a valid-address path.
	valid := VerifyLocalPassword(stored, password)
	if !validPassword(password) {
		_ = VerifyLocalPassword(unknownAccountHash(), "invalid padded credential")
	}
	if !valid || userID == "" || credentialVerified == nil || !profileVerified || deleted != nil {
		if err := tx.Commit(ctx); err != nil {
			return LocalLoginTokens{}, err
		}
		return LocalLoginTokens{}, ErrInvalidCredential
	}
	access, err := s.Codec.signVerified(userID, accessVersion, now)
	if err != nil {
		return LocalLoginTokens{}, err
	}
	refresh, refreshHash, err := NewLocalOpaqueToken("refresh")
	if err != nil {
		return LocalLoginTokens{}, err
	}
	refreshExpiry := now.UTC().Add(localRefreshLifetime)
	if _, err = tx.Exec(ctx, `INSERT INTO auth_refresh_sessions(user_id,family_id,token_hash,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5)`, userID, uuid.NewString(), refreshHash[:], now.UTC(), refreshExpiry); err != nil {
		return LocalLoginTokens{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LocalLoginTokens{}, err
	}
	return LocalLoginTokens{AccessToken: access, AccessExpiresAt: now.UTC().Add(localAccessLifetime), RefreshToken: refresh, RefreshExpiresAt: refreshExpiry}, nil
}

// Reject accidental future use of a whitespace-only source key. The HTTP
// adapter will parse the transport peer and normalize its address separately.
func validLocalAuthSource(source string) bool {
	return source != "" && source == strings.TrimSpace(source) && len(source) <= 128
}
