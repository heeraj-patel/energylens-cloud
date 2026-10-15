package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

// LocalIssuer is separate from the configured external OIDC issuer. A local
// signup must insert this issuer and its stable user UUID into auth_identities.
const LocalIssuer = "urn:energylens:local:v1"
const LocalUsernameIssuer = "urn:energylens:runtime-local:v1"

// LocalAuthProfile is selected by the server at startup, never by a request.
type LocalAuthProfile string

const (
	LocalEmailProfile    LocalAuthProfile = "email"
	LocalUsernameProfile LocalAuthProfile = "username_local"
)

func (p LocalAuthProfile) valid() bool {
	return p == "" || p == LocalEmailProfile || p == LocalUsernameProfile
}
func (p LocalAuthProfile) issuer() string {
	if p == LocalUsernameProfile {
		return LocalUsernameIssuer
	}
	return LocalIssuer
}
func (p LocalAuthProfile) kind() string {
	if p == LocalUsernameProfile {
		return string(LocalUsernameProfile)
	}
	return string(LocalEmailProfile)
}
func (p LocalAuthProfile) eligible(verified, activated *time.Time, emailVerified bool) bool {
	if p == LocalUsernameProfile {
		return activated != nil && verified == nil && !emailVerified
	}
	return verified != nil && emailVerified && activated == nil
}

const (
	localAccessLifetime = 15 * time.Minute
	passwordMemoryKiB   = 64 * 1024
	passwordIterations  = 3
	passwordThreads     = 1
)

var ErrInvalidCredential = errors.New("invalid credential")
var localEmailPattern = regexp.MustCompile(`^[A-Za-z0-9_+%-]+(?:\.[A-Za-z0-9_+%-]+)*@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)
var localUsernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func NormalizeLocalUsername(raw string) (string, error) {
	normalized := strings.Trim(raw, " ")
	for i := 0; i < len(normalized); i++ {
		if normalized[i] >= 'A' && normalized[i] <= 'Z' {
			normalized = normalized[:i] + string(normalized[i]+('a'-'A')) + normalized[i+1:]
		}
	}
	if !localUsernamePattern.MatchString(normalized) {
		return "", ErrInvalidCredential
	}
	return normalized, nil
}

func NormalizeLocalEmail(raw string) (string, error) {
	if len(raw) > 254 || !localEmailPattern.MatchString(raw) {
		return "", ErrInvalidCredential
	}
	parts := strings.Split(raw, "@")
	if len(parts[0]) > 64 {
		return "", ErrInvalidCredential
	}
	for _, label := range strings.Split(parts[1], ".") {
		if len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", ErrInvalidCredential
		}
	}
	return strings.ToLower(raw), nil
}

func validPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024 && utf8.ValidString(password) && !strings.ContainsRune(password, 0)
}

// HashLocalPassword stores only a bounded Argon2id PHC string. The password
// policy and verification errors do not distinguish an existing account.
func HashLocalPassword(password string) (string, error) {
	if !validPassword(password) {
		return "", ErrInvalidCredential
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemoryKiB, passwordThreads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemoryKiB, passwordIterations, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest)), nil
}

func VerifyLocalPassword(stored, password string) bool {
	if !validPassword(password) || len(stored) > 255 {
		return false
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=1" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 || base64.RawStdEncoding.EncodeToString(salt) != parts[4] {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != 32 || base64.RawStdEncoding.EncodeToString(want) != parts[5] {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemoryKiB, passwordThreads, 32)
	return subtle.ConstantTimeCompare(actual, want) == 1
}

// NewLocalOpaqueToken returns an unguessable bearer value and a domain-separated
// hash for database storage. Only the hash belongs in verification/session rows.
func NewLocalOpaqueToken(purpose string) (string, [32]byte, error) {
	if purpose != "verify_email" && purpose != "refresh" && purpose != "reset_password" {
		return "", [32]byte{}, ErrInvalidCredential
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", [32]byte{}, err
	}
	raw := base64.RawURLEncoding.EncodeToString(secret)
	hash, err := HashLocalOpaqueToken(purpose, raw)
	return raw, hash, err
}

func HashLocalOpaqueToken(purpose, raw string) ([32]byte, error) {
	if purpose != "verify_email" && purpose != "refresh" && purpose != "reset_password" || len(raw) != 43 {
		return [32]byte{}, ErrInvalidCredential
	}
	secret, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != raw {
		return [32]byte{}, ErrInvalidCredential
	}
	return sha256.Sum256(append([]byte("energylens.local-auth."+purpose+".v1\n"), secret...)), nil
}

// LocalAccessCodec signs with one active key and verifies only explicit key IDs
// in a bounded ring. Key material is provisioned outside the service.
type LocalAccessCodec struct {
	Private     ed25519.PrivateKey
	ActiveKeyID string
	Keys        map[string]ed25519.PublicKey
	Audience    string
	Profile     LocalAuthProfile
}

func (c LocalAccessCodec) validVerification() bool {
	if c.Audience == "" || !c.Profile.valid() || len(c.Keys) == 0 || len(c.Keys) > 8 {
		return false
	}
	for id, key := range c.Keys {
		if !validLocalKeyID(id) || len(key) != ed25519.PublicKeySize {
			return false
		}
	}
	return true
}

func (c LocalAccessCodec) valid() bool {
	if !c.validVerification() || len(c.Private) != ed25519.PrivateKeySize || !validLocalKeyID(c.ActiveKeyID) {
		return false
	}
	return subtle.ConstantTimeCompare(c.Private.Public().(ed25519.PublicKey), c.Keys[c.ActiveKeyID]) == 1
}

var localKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validLocalKeyID(id string) bool { return localKeyIDPattern.MatchString(id) }

type localAccessClaims struct {
	Version   int64            `json:"ver"`
	AuthTime  *jwt.NumericDate `json:"auth_time,omitempty"`
	SessionID string           `json:"sid,omitempty"`
	jwt.RegisteredClaims
}

// signVerified is package-private so only the future verified-credential
// service can issue a token after checking its database state.
func (c LocalAccessCodec) signVerified(userID string, version int64, now time.Time) (string, error) {
	return c.signVerifiedAt(userID, version, now, now)
}

func (c LocalAccessCodec) signVerifiedAt(userID string, version int64, now, authTime time.Time) (string, error) {
	return c.signVerifiedSessionAt(userID, version, "", now, authTime)
}

func (c LocalAccessCodec) signVerifiedSessionAt(userID string, version int64, familyID string, now, authTime time.Time) (string, error) {
	if !c.valid() || !canonicalLocalUserID(userID) || version < 1 || now.IsZero() {
		return "", ErrInvalidCredential
	}
	if c.Profile == LocalUsernameProfile && !canonicalLocalUserID(familyID) || c.Profile != LocalUsernameProfile && familyID != "" {
		return "", ErrInvalidCredential
	}
	if authTime.IsZero() || authTime.After(now.Add(time.Minute)) || now.Sub(authTime) > localRefreshLifetime {
		return "", ErrInvalidCredential
	}
	claims := localAccessClaims{Version: version, AuthTime: jwt.NewNumericDate(authTime.UTC()), SessionID: familyID, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: c.Profile.issuer(), Subject: userID, Audience: jwt.ClaimStrings{c.Audience}, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(now.UTC()), NotBefore: jwt.NewNumericDate(now.UTC()), ExpiresAt: jwt.NewNumericDate(now.UTC().Add(localAccessLifetime)),
	}}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = c.ActiveKeyID
	return token.SignedString(c.Private)
}

func (c LocalAccessCodec) verifySignature(raw string) (string, int64, error) {
	userID, version, _, err := c.verifySignatureWithAuthTime(raw)
	return userID, version, err
}

func (c LocalAccessCodec) verifySignatureWithAuthTime(raw string) (string, int64, time.Time, error) {
	userID, version, authTime, _, err := c.verifySignatureWithSession(raw)
	return userID, version, authTime, err
}

func (c LocalAccessCodec) verifySignatureWithSession(raw string) (string, int64, time.Time, string, error) {
	if !c.validVerification() || len(raw) > 4096 {
		return "", 0, time.Time{}, "", ErrInvalidCredential
	}
	claims := &localAccessClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || !validLocalKeyID(kid) || c.Keys[kid] == nil {
			return nil, ErrInvalidCredential
		}
		return c.Keys[kid], nil
	},
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(c.Profile.issuer()), jwt.WithAudience(c.Audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || !canonicalLocalUserID(claims.Subject) || claims.ID == "" {
		return "", 0, time.Time{}, "", ErrInvalidCredential
	}
	if c.Profile == LocalUsernameProfile && !canonicalLocalUserID(claims.SessionID) || c.Profile != LocalUsernameProfile && claims.SessionID != "" {
		return "", 0, time.Time{}, "", ErrInvalidCredential
	}
	if claims.Version < 1 || claims.ExpiresAt == nil || claims.IssuedAt == nil || claims.NotBefore == nil ||
		claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) != localAccessLifetime || claims.IssuedAt.Time.After(time.Now().Add(time.Minute)) {
		return "", 0, time.Time{}, "", ErrInvalidCredential
	}
	var authTime time.Time
	if claims.AuthTime != nil {
		authTime = claims.AuthTime.Time
		if authTime.After(claims.IssuedAt.Time.Add(time.Minute)) || claims.IssuedAt.Time.Sub(authTime) > localRefreshLifetime {
			return "", 0, time.Time{}, "", ErrInvalidCredential
		}
	}
	return claims.Subject, claims.Version, authTime, claims.SessionID, nil
}

// LocalAccessVerifier must be used at a future HTTP edge: signature checks
// alone cannot enforce reset, verification, and deletion lifecycle state.
type LocalAccessVerifier struct {
	DB    *pgxpool.Pool
	Codec LocalAccessCodec
}

func (v LocalAccessVerifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if v.DB == nil {
		return Identity{}, ErrInvalidCredential
	}
	userID, version, authTime, sessionID, err := v.Codec.verifySignatureWithSession(raw)
	if err != nil {
		return Identity{}, ErrInvalidCredential
	}
	var current int64
	var verified, activated *time.Time
	var emailVerified bool
	err = v.DB.QueryRow(ctx, `SELECT c.access_version,c.verified_at,c.activated_at,u.email_verified FROM auth_password_credentials c
		JOIN users u ON u.id=c.user_id JOIN auth_identities i ON i.user_id=u.id AND i.issuer=$2 AND i.subject=u.id::text
		WHERE c.user_id=$1 AND c.credential_kind=$3 AND u.deleted_at IS NULL
		AND ($3 <> 'username_local' OR EXISTS (
			SELECT 1 FROM auth_refresh_sessions s
			WHERE s.user_id=c.user_id AND s.family_id=$4::uuid AND s.revoked_at IS NULL AND s.expires_at>now()
		))`, userID, v.Codec.Profile.issuer(), v.Codec.Profile.kind(), nullableSessionID(sessionID)).Scan(&current, &verified, &activated, &emailVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrInvalidCredential
	}
	if err != nil {
		return Identity{}, err
	}
	if current != version || !v.Codec.Profile.eligible(verified, activated, emailVerified) {
		return Identity{}, ErrInvalidCredential
	}
	return Identity{Issuer: v.Codec.Profile.issuer(), Subject: userID, EmailVerified: v.Codec.Profile != LocalUsernameProfile, AuthTime: authTime, SessionID: sessionID}, nil
}

func nullableSessionID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func canonicalLocalUserID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}
