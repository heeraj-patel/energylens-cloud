package hubtrust

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrPairingRateLimited = errors.New("pairing lookup rate limited")
var ErrPairingUnavailable = errors.New("pairing directory unavailable")

const pairingTTL = 15 * time.Minute
const maxActivePairingCodes = 100

var pairingCodeFormat = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{6}$`)
var enrollmentSecretFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PairingDirectory uses independently provisioned key material, never auth or
// signing keys. Codes, request fingerprints, counters and encryption have
// separate HMAC domains. No secret enters generic request replay storage.
type PairingDirectory struct {
	Store Store
	Key   []byte
}

type PairingPublication struct {
	PublicationID     string    `json:"publication_id"`
	Code              string    `json:"code"`
	EnrollmentSecret  string    `json:"enrollment_secret"`
	HTTPSOrigin       string    `json:"https_origin"`
	CertificateSHA256 string    `json:"certificate_sha256"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type PairingResolution struct {
	HubID             string    `json:"hub_id"`
	LocalHubID        string    `json:"local_hub_id"`
	HTTPSOrigin       string    `json:"https_origin"`
	CertificateSHA256 string    `json:"certificate_sha256"`
	EnrollmentSecret  string    `json:"enrollment_secret"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func normalizePairingCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (d PairingDirectory) configured() bool { return d.Store.Pool != nil && len(d.Key) == 32 }
func (d PairingDirectory) hash(domain, value string) []byte {
	h := hmac.New(sha256.New, d.Key)
	h.Write([]byte("energylens.hub-pairing." + domain + ".v1\n"))
	h.Write([]byte(value))
	return h.Sum(nil)
}

func (d PairingDirectory) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(d.hash("encryption-key", ""))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func pairingAAD(hub, publication string, expiry time.Time) []byte {
	return []byte("energylens.hub-pairing.enrollment.v1\n" + hub + "\n" + publication + "\n" + expiry.UTC().Format(time.RFC3339Nano))
}

func (d PairingDirectory) seal(secret string, aad []byte) ([]byte, error) {
	aead, err := d.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(secret), aad), nil
}

func (d PairingDirectory) open(sealed, aad []byte) (string, error) {
	aead, err := d.aead()
	if err != nil || len(sealed) < aead.NonceSize() {
		return "", ErrPairingUnavailable
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return "", ErrPairingUnavailable
	}
	return string(plain), nil
}

// Publish accepts only a current registered, unclaimed Hub's mTLS identity.
// Repeating an identical publication never extends its original expiry.
func (d PairingDirectory) Publish(ctx context.Context, hub, serial string, epoch int64, in PairingPublication) (time.Time, error) {
	if !d.configured() {
		return time.Time{}, ErrPairingUnavailable
	}
	now := time.Now().UTC()
	in.Code = normalizePairingCode(in.Code)
	if !uuidOK(in.PublicationID) || !pairingCodeFormat.MatchString(in.Code) ||
		!enrollmentSecretFormat.MatchString(in.EnrollmentSecret) || !validConnection(in.HTTPSOrigin, in.CertificateSHA256) ||
		!in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(pairingTTL)) {
		return time.Time{}, ErrDenied
	}
	encoded, _ := json.Marshal(in)
	fingerprint := d.hash("publication", string(encoded))
	tx, err := d.Store.Pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	// A bounded directory plus a durable global guess budget bounds the chance
	// of guessing any live 30-bit code, including across many accounts/IPs.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(187882026)`); err != nil {
		return time.Time{}, err
	}
	var certExpiry time.Time
	err = tx.QueryRow(ctx, `SELECT c.expires_at FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id
		WHERE b.id=$1 AND b.ownership_epoch=$3 AND b.home_id IS NULL AND b.revoked_at IS NULL
		AND c.serial=$2 AND c.ownership_epoch=b.ownership_epoch AND c.revoked_at IS NULL AND c.expires_at>now()
		FOR UPDATE OF b`, hub, serial, epoch).Scan(&certExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrDenied
	}
	if err != nil {
		return time.Time{}, err
	}
	var priorHash []byte
	var priorExpiry time.Time
	var priorSerial string
	err = tx.QueryRow(ctx, `SELECT request_hash,expires_at,publisher_cert_serial FROM hub_pairing_codes WHERE hub_id=$1 AND publication_id=$2`, hub, in.PublicationID).Scan(&priorHash, &priorExpiry, &priorSerial)
	if err == nil {
		if !bytes.Equal(priorHash, fingerprint) || priorSerial != serial || !priorExpiry.After(now) {
			return time.Time{}, ErrConflict
		}
		return priorExpiry, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	// A resolved code has handed out the one-use local capability. Keep that
	// reservation intact until it expires or the Hub is claimed.
	var reserved bool
	err = tx.QueryRow(ctx, `SELECT resolved_owner_user_id IS NOT NULL FROM hub_pairing_codes WHERE hub_id=$1 AND expires_at>now()`, hub).Scan(&reserved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	if err == nil && reserved {
		return time.Time{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM hub_pairing_publication_ids WHERE expires_at<=now()`); err != nil {
		return time.Time{}, err
	}
	registered, err := tx.Exec(ctx, `INSERT INTO hub_pairing_publication_ids(publication_id,hub_id,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, in.PublicationID, hub, in.ExpiresAt)
	if err != nil {
		return time.Time{}, err
	}
	if registered.RowsAffected() != 1 {
		return time.Time{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM hub_pairing_codes WHERE expires_at<=now()`); err != nil {
		return time.Time{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM hub_pairing_codes WHERE hub_id<>$1`, hub).Scan(&count); err != nil {
		return time.Time{}, err
	}
	if count >= maxActivePairingCodes {
		return time.Time{}, ErrPairingRateLimited
	}
	expiry := in.ExpiresAt.UTC()
	if certExpiry.Before(expiry) {
		expiry = certExpiry
	}
	// PostgreSQL timestamps retain microseconds; bind ciphertext to exactly the
	// representation that will be read back from the database.
	expiry = expiry.Truncate(time.Microsecond)
	sealed, err := d.seal(in.EnrollmentSecret, pairingAAD(hub, in.PublicationID, expiry))
	if err != nil {
		return time.Time{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO hub_pairing_codes(hub_id,publication_id,ownership_epoch,publisher_cert_serial,code_hash,request_hash,enrollment_ciphertext,https_origin,certificate_sha256,published_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),$10)
		ON CONFLICT(hub_id) DO UPDATE SET publication_id=excluded.publication_id,ownership_epoch=excluded.ownership_epoch,
		publisher_cert_serial=excluded.publisher_cert_serial,code_hash=excluded.code_hash,request_hash=excluded.request_hash,
		enrollment_ciphertext=excluded.enrollment_ciphertext,https_origin=excluded.https_origin,certificate_sha256=excluded.certificate_sha256,
		published_at=excluded.published_at,expires_at=excluded.expires_at,
		resolved_owner_user_id=NULL,resolved_home_id=NULL,resolved_session_id=NULL,resolved_client_id=NULL,resolve_operation_id=NULL`,
		hub, in.PublicationID, epoch, serial, d.hash("code", in.Code), fingerprint, sealed, in.HTTPSOrigin, in.CertificateSHA256, expiry)
	if isPGCode(err, "23505") {
		return time.Time{}, ErrConflict
	}
	if err != nil {
		return time.Time{}, err
	}
	return expiry, tx.Commit(ctx)
}

// AllowResolve counts every authenticated lookup before parsing/validating its
// body. Its transaction commits independently, so denied guesses survive both
// request rollback and API restarts. It trusts only the configured source policy.
func (d PairingDirectory) AllowResolve(ctx context.Context, owner, source string) error {
	if !d.configured() {
		return ErrPairingUnavailable
	}
	tx, err := d.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	allowed := true
	for _, rate := range []struct {
		scope, value string
		limit        int
	}{
		{"global", "all", 100}, {"owner", owner, 5}, {"source", source, 20},
	} {
		var attempts int
		err = tx.QueryRow(ctx, `INSERT INTO hub_pairing_rate_limits(scope,key_hash,window_start,attempts) VALUES($1,$2,now(),1)
			ON CONFLICT(scope,key_hash) DO UPDATE SET
			attempts=CASE WHEN hub_pairing_rate_limits.window_start<=now()-interval '15 minutes' THEN 1 ELSE LEAST(hub_pairing_rate_limits.attempts+1,1000000) END,
			window_start=CASE WHEN hub_pairing_rate_limits.window_start<=now()-interval '15 minutes' THEN now() ELSE hub_pairing_rate_limits.window_start END RETURNING attempts`,
			rate.scope, d.hash("rate-"+rate.scope, rate.value)).Scan(&attempts)
		if err != nil {
			return err
		}
		allowed = allowed && attempts <= rate.limit
	}
	if _, err = tx.Exec(ctx, `DELETE FROM hub_pairing_rate_limits WHERE window_start<now()-interval '1 day'`); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if !allowed {
		return ErrPairingRateLimited
	}
	return nil
}

// Resolve reserves a code for one authenticated account/home/session/install
// and operation. An exact retry is permitted only while all original trust and
// expiry checks still hold. Another resolver sees the same generic miss.
func (d PairingDirectory) Resolve(ctx context.Context, owner, home, session, client, operation, code string) (PairingResolution, error) {
	var out PairingResolution
	if !d.configured() {
		return out, ErrPairingUnavailable
	}
	code = normalizePairingCode(code)
	if !uuidOK(owner) || !uuidOK(home) || session == "" || len(session) > 256 || !uuidOK(client) || !uuidOK(operation) || !pairingCodeFormat.MatchString(code) {
		return out, ErrNotFound
	}
	tx, err := d.Store.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var publication string
	var sealed []byte
	var boundOwner, boundHome, boundSession, boundClient, boundOperation *string
	err = tx.QueryRow(ctx, `SELECT b.id,b.local_hub_id,p.publication_id,p.https_origin,p.certificate_sha256,p.enrollment_ciphertext,p.expires_at,
		p.resolved_owner_user_id,p.resolved_home_id,p.resolved_session_id,p.resolved_client_id,p.resolve_operation_id
		FROM hub_pairing_codes p JOIN hubs b ON b.id=p.hub_id
		JOIN hub_certificates c ON c.serial=p.publisher_cert_serial AND c.hub_id=b.id
		JOIN homes h ON h.id=$2 AND h.owner_user_id=$1 AND h.deleted_at IS NULL
		JOIN users u ON u.id=h.owner_user_id AND u.deleted_at IS NULL
		WHERE p.code_hash=$3 AND p.expires_at>now() AND b.home_id IS NULL AND b.revoked_at IS NULL
		AND b.ownership_epoch=p.ownership_epoch AND c.ownership_epoch=b.ownership_epoch AND c.revoked_at IS NULL AND c.expires_at>now()
		FOR UPDATE OF b,p`, owner, home, d.hash("code", code)).Scan(&out.HubID, &out.LocalHubID, &publication, &out.HTTPSOrigin, &out.CertificateSHA256, &sealed, &out.ExpiresAt,
		&boundOwner, &boundHome, &boundSession, &boundClient, &boundOperation)
	if errors.Is(err, pgx.ErrNoRows) {
		return PairingResolution{}, ErrNotFound
	}
	if err != nil {
		return PairingResolution{}, err
	}
	if boundOwner != nil && (*boundOwner != owner || *boundHome != home || *boundSession != session || *boundClient != client || *boundOperation != operation) {
		return PairingResolution{}, ErrNotFound
	}
	if boundOwner == nil {
		_, err = tx.Exec(ctx, `UPDATE hub_pairing_codes SET resolved_owner_user_id=$2,resolved_home_id=$3,resolved_session_id=$4,resolved_client_id=$5,resolve_operation_id=$6 WHERE hub_id=$1`, out.HubID, owner, home, session, client, operation)
		if err != nil {
			return PairingResolution{}, err
		}
	}
	out.EnrollmentSecret, err = d.open(sealed, pairingAAD(out.HubID, publication, out.ExpiresAt))
	if err != nil {
		return PairingResolution{}, fmt.Errorf("open Hub pairing capability: %w", ErrPairingUnavailable)
	}
	if err = tx.Commit(ctx); err != nil {
		return PairingResolution{}, err
	}
	return out, nil
}
