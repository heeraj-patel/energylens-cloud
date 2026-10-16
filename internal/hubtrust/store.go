package hubtrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDenied = errors.New("hub trust denied")
var ErrConflict = errors.New("hub ownership conflict")
var ErrExpired = errors.New("challenge expired or consumed")
var ErrNotFound = errors.New("not found")
var ErrKeyReuse = errors.New("idempotency key reused")

const challengeTTL = 5 * time.Minute
const enrollTTL = 15 * time.Minute

func isPGCode(err error, codes ...string) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	for _, code := range codes {
		if pg.Code == code {
			return true
		}
	}
	return false
}
func claimDBError(err error) error {
	if isPGCode(err, "40001", "40P01") {
		return ErrConflict
	}
	return err
}
func readReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte, out any) (bool, error) {
	var oldHash, body []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response_json FROM hub_request_idempotency WHERE owner_user_id=$1 AND operation=$2 AND key=$3`, owner, op, key).Scan(&oldHash, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(oldHash, hash) {
		return true, ErrKeyReuse
	}
	return true, json.Unmarshal(body, out)
}
func saveReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte, v any, status int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO hub_request_idempotency(owner_user_id,operation,key,request_hash,response_json,response_status) VALUES($1,$2,$3,$4,$5,$6)`, owner, op, key, hash, body, status)
	return err
}

type Store struct{ Pool *pgxpool.Pool }
type Hub struct {
	ID      string
	LocalID string
	Key     ed25519.PublicKey
	HomeID  *string
	Epoch   int64
	Version int64
	Revoked bool
}
type Challenge struct {
	ClaimID    string    `json:"claim_id"`
	HubID      string    `json:"hub_id"`
	LocalHubID string    `json:"local_hub_id"`
	Nonce      string    `json:"nonce"`
	ExpiresAt  time.Time `json:"expires_at"`
}
type Ownership struct {
	HubID      string      `json:"hub_id"`
	LocalHubID string      `json:"local_hub_id"`
	HomeID     string      `json:"home_id"`
	Epoch      int64       `json:"ownership_epoch"`
	Version    int64       `json:"version"`
	Connection *Connection `json:"connection,omitempty"`
}

// AuthorizeOwner derives the complete Hub/home tuple from current database
// rows. A prior-epoch certificate is valid for renewal only, never for owner
// authorization. Foreign and missing homes deliberately share one result.
func (s Store) AuthorizeOwner(ctx context.Context, hubID, serial string, peerEpoch int64, ownerID, homeID string) (Ownership, error) {
	var result Ownership
	err := s.Pool.QueryRow(ctx, `SELECT b.id,b.local_hub_id,b.home_id,b.ownership_epoch,b.version
		FROM hubs b
		JOIN homes h ON h.id=b.home_id AND h.owner_user_id=$5 AND h.deleted_at IS NULL
		JOIN users u ON u.id=h.owner_user_id AND u.deleted_at IS NULL
		JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=b.ownership_epoch
			AND c.revoked_at IS NULL AND c.expires_at>now()
		WHERE b.id=$1 AND b.home_id=$4 AND b.ownership_epoch=$3 AND b.revoked_at IS NULL`,
		hubID, serial, peerEpoch, homeID, ownerID,
	).Scan(&result.HubID, &result.LocalHubID, &result.HomeID, &result.Epoch, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ownership{}, ErrNotFound
	}
	if err != nil {
		return Ownership{}, err
	}
	return result, nil
}

// ListOwner returns only the current bindings for an owned, active home.
// A missing or foreign home has the same result to avoid exposing hub IDs.
func (s Store) ListOwner(ctx context.Context, owner, home string) ([]Ownership, error) {
	var owned bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, home, owner).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrDenied
	}
	rows, err := s.Pool.Query(ctx, `SELECT b.id,b.local_hub_id,b.home_id,b.ownership_epoch,b.version,
		x.https_origin,x.certificate_sha256,x.expires_at
		FROM hubs b JOIN homes h ON h.id=b.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
		JOIN users u ON u.id=h.owner_user_id AND u.deleted_at IS NULL
		LEFT JOIN hub_connections x ON x.hub_id=b.id AND x.home_id=b.home_id
			AND x.ownership_epoch=b.ownership_epoch AND x.expires_at>now()
			AND EXISTS(SELECT 1 FROM hub_certificates c WHERE c.serial=x.publisher_cert_serial
				AND c.hub_id=b.id AND c.ownership_epoch=b.ownership_epoch
				AND c.revoked_at IS NULL AND c.expires_at>now())
		WHERE b.home_id=$1 AND b.revoked_at IS NULL ORDER BY b.id`, home, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ownership{}
	for rows.Next() {
		var binding Ownership
		var origin, pin *string
		var expires *time.Time
		if err := rows.Scan(&binding.HubID, &binding.LocalHubID, &binding.HomeID, &binding.Epoch, &binding.Version, &origin, &pin, &expires); err != nil {
			return nil, err
		}
		if origin != nil && pin != nil && expires != nil {
			binding.Connection = &Connection{HTTPSOrigin: *origin, CertificateSHA256: *pin, ExpiresAt: *expires}
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decode64(s string, n int) ([]byte, error) {
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil || len(b) != n || b64(b) != s {
		return nil, ErrDenied
	}
	return b, nil
}
func random32() ([]byte, error) { b := make([]byte, 32); _, e := rand.Read(b); return b, e }
func shaHex(s string) string    { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func claimTranscript(id, local, nonce, proof string) []byte {
	return []byte("energylens.hub.claim.v1\n" + id + "\n" + local + "\n" + nonce + "\n" + shaHex(proof) + "\n")
}
func enrollTranscript(hub, local, nonce, signedAt, csrHash string) []byte {
	return []byte("energylens.hub.enroll.v1\n" + hub + "\n" + local + "\n" + nonce + "\n" + signedAt + "\n" + csrHash + "\n")
}
func (s Store) Provision(ctx context.Context, local string, key ed25519.PublicKey, actor string) (string, string, error) {
	if len(key) != ed25519.PublicKeySize || actor == "" || !validLocal(local) {
		return "", "", ErrDenied
	}
	nonce, e := random32()
	if e != nil {
		return "", "", e
	}
	h := sha256.Sum256(nonce)
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback(ctx)
	var id string
	e = tx.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by) VALUES($1,$2,$3) RETURNING id`, local, []byte(key), actor).Scan(&id)
	if e != nil {
		return "", "", e
	}
	_, e = tx.Exec(ctx, `INSERT INTO hub_enrollment_nonces(nonce_hash,hub_id,expires_at) VALUES($1,$2,now()+interval '15 minutes')`, h[:], id)
	if e != nil {
		return "", "", e
	}
	return id, b64(nonce), tx.Commit(ctx)
}
func validLocal(s string) bool {
	if len(s) < 7 || len(s) > 128 || s[:6] != "elhub_" {
		return false
	}
	for _, c := range s[6:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (s Store) Begin(ctx context.Context, owner, home, local, requestKey string) (Challenge, error) {
	if !validLocal(local) {
		return Challenge{}, ErrNotFound
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return Challenge{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); e != nil {
		return Challenge{}, e
	}
	hash := sha256.Sum256([]byte(home + "\n" + local))
	var old Challenge
	if found, e := readReplay(ctx, tx, owner, "begin_claim", requestKey, hash[:], &old); found || e != nil {
		return old, e
	}
	var hubID string
	var revoked bool
	e = tx.QueryRow(ctx, `SELECT b.id,b.revoked_at IS NOT NULL FROM hubs b JOIN homes h ON h.id=$2 AND h.owner_user_id=$1 AND h.deleted_at IS NULL WHERE b.local_hub_id=$3`, owner, home, local).Scan(&hubID, &revoked)
	if errors.Is(e, pgx.ErrNoRows) || revoked {
		return Challenge{}, ErrNotFound
	}
	if e != nil {
		return Challenge{}, e
	}
	nonce, e := random32()
	if e != nil {
		return Challenge{}, e
	}
	c := Challenge{HubID: hubID, LocalHubID: local, Nonce: b64(nonce)}
	e = tx.QueryRow(ctx, `INSERT INTO hub_claims(hub_id,home_id,owner_user_id,nonce,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes') RETURNING id,expires_at`, hubID, home, owner, nonce).Scan(&c.ClaimID, &c.ExpiresAt)
	if e != nil {
		return Challenge{}, e
	}
	if e = saveReplay(ctx, tx, owner, "begin_claim", requestKey, hash[:], c, 201); e != nil {
		return Challenge{}, e
	}
	return c, tx.Commit(ctx)
}
func (s Store) Finish(ctx context.Context, owner, claimID, local, proof, signature, requestKey string) (Ownership, error) {
	sig, e := decode64(signature, ed25519.SignatureSize)
	if e != nil {
		return Ownership{}, ErrDenied
	}
	if _, e = decode64(proof, 32); e != nil || !validLocal(local) {
		return Ownership{}, ErrDenied
	}
	tx, e := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if e != nil {
		return Ownership{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); e != nil {
		return Ownership{}, e
	}
	hash := sha256.Sum256([]byte(claimID + "\n" + local + "\n" + proof + "\n" + signature))
	var old Ownership
	if found, e := readReplay(ctx, tx, owner, "finish_claim", requestKey, hash[:], &old); found || e != nil {
		return old, e
	}
	var hubID, homeID, claimOwner, registeredLocal string
	var nonce, key []byte
	var expiry time.Time
	var consumed *time.Time
	var revoked bool
	e = tx.QueryRow(ctx, `SELECT c.hub_id,c.home_id,c.owner_user_id,c.nonce,c.expires_at,c.consumed_at,b.local_hub_id,b.device_public_key,b.revoked_at IS NOT NULL FROM hub_claims c JOIN hubs b ON b.id=c.hub_id WHERE c.id=$1 FOR UPDATE OF c,b`, claimID).Scan(&hubID, &homeID, &claimOwner, &nonce, &expiry, &consumed, &registeredLocal, &key, &revoked)
	if errors.Is(e, pgx.ErrNoRows) {
		return Ownership{}, ErrNotFound
	}
	if e != nil {
		return Ownership{}, e
	}
	if consumed != nil || !time.Now().Before(expiry) {
		return Ownership{}, ErrExpired
	}
	if claimOwner != owner || registeredLocal != local || revoked {
		return Ownership{}, ErrDenied
	}
	var homeOwner string
	e = tx.QueryRow(ctx, `SELECT owner_user_id FROM homes WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, homeID).Scan(&homeOwner)
	if e != nil || homeOwner != owner {
		return Ownership{}, ErrDenied
	}
	if !ed25519.Verify(key, claimTranscript(claimID, local, b64(nonce), proof), sig) {
		return Ownership{}, ErrDenied
	}
	proofHash := sha256.Sum256([]byte(proof))
	_, e = tx.Exec(ctx, `INSERT INTO hub_possession_proofs(proof_hash,claim_id) VALUES($1,$2)`, proofHash[:], claimID)
	if e != nil {
		if isPGCode(e, "23505") {
			return Ownership{}, ErrExpired
		}
		return Ownership{}, claimDBError(e)
	}
	var o Ownership
	o.HubID = hubID
	o.LocalHubID = local
	o.HomeID = homeID
	e = tx.QueryRow(ctx, `UPDATE hubs SET home_id=$2,ownership_epoch=ownership_epoch+1,version=version+1 WHERE id=$1 AND home_id IS NULL AND revoked_at IS NULL RETURNING ownership_epoch,version`, hubID, homeID).Scan(&o.Epoch, &o.Version)
	if errors.Is(e, pgx.ErrNoRows) {
		return Ownership{}, ErrConflict
	}
	if e != nil {
		return Ownership{}, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO hub_ownership_history(hub_id,ownership_epoch,home_id,owner_user_id) VALUES($1,$2,$3,$4)`, hubID, o.Epoch, homeID, owner)
	if e != nil {
		return Ownership{}, e
	}
	_, e = tx.Exec(ctx, `UPDATE hub_claims SET consumed_at=now() WHERE id=$1`, claimID)
	if e != nil {
		return Ownership{}, e
	}
	if e = saveReplay(ctx, tx, owner, "finish_claim", requestKey, hash[:], o, 200); e != nil {
		return Ownership{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Ownership{}, claimDBError(fmt.Errorf("commit hub claim: %w", e))
	}
	return o, nil
}
func (s Store) Unclaim(ctx context.Context, owner, home, hub string, version int64, requestKey string) error {
	tx, e := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); e != nil {
		return e
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", home, hub, version)))
	var old map[string]any
	if found, e := readReplay(ctx, tx, owner, "unclaim_hub", requestKey, hash[:], &old); found || e != nil {
		return e
	}
	var epoch, current int64
	e = tx.QueryRow(ctx, `SELECT b.ownership_epoch,b.version FROM hubs b JOIN homes h ON h.id=b.home_id AND h.owner_user_id=$1 AND h.deleted_at IS NULL WHERE b.id=$2 AND b.home_id=$3 FOR UPDATE OF b`, owner, hub, home).Scan(&epoch, &current)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if e != nil {
		return e
	}
	if version != current {
		return ErrConflict
	}
	_, e = tx.Exec(ctx, `UPDATE hub_ownership_history SET ended_at=now() WHERE hub_id=$1 AND ownership_epoch=$2 AND ended_at IS NULL`, hub, epoch)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE hub_certificates SET revoked_at=now() WHERE hub_id=$1 AND revoked_at IS NULL`, hub)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=ownership_epoch+1,version=version+1 WHERE id=$1`, hub)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE hub_claims SET consumed_at=now() WHERE hub_id=$1 AND consumed_at IS NULL`, hub)
	if e != nil {
		return e
	}
	if e = saveReplay(ctx, tx, owner, "unclaim_hub", requestKey, hash[:], map[string]any{}, 204); e != nil {
		return e
	}
	return claimDBError(tx.Commit(ctx))
}
func (s Store) Enrollment(ctx context.Context, hubID, local, nonce, signedAt string, csrDER, sig []byte) (Hub, error) {
	n, e := decode64(nonce, 32)
	if e != nil {
		return Hub{}, ErrDenied
	}
	h := sha256.Sum256(n)
	csrHash := sha256.Sum256(csrDER)
	var b Hub
	var key []byte
	var expiry time.Time
	var consumed *time.Time
	e = s.Pool.QueryRow(ctx, `SELECT b.id,b.local_hub_id,b.device_public_key,b.ownership_epoch,b.version,b.revoked_at IS NOT NULL,n.expires_at,n.consumed_at FROM hubs b JOIN hub_enrollment_nonces n ON n.hub_id=b.id AND n.nonce_hash=$2 WHERE b.id=$1`, hubID, h[:]).Scan(&b.ID, &b.LocalID, &key, &b.Epoch, &b.Version, &b.Revoked, &expiry, &consumed)
	if errors.Is(e, pgx.ErrNoRows) {
		return Hub{}, ErrDenied
	}
	if e != nil {
		return Hub{}, e
	}
	if b.Revoked || b.LocalID != local || !time.Now().Before(expiry) {
		return Hub{}, ErrDenied
	}
	ts, e := time.Parse(time.RFC3339, signedAt)
	if e != nil || time.Since(ts) > enrollTTL || time.Until(ts) > time.Minute {
		return Hub{}, ErrDenied
	}
	if !ed25519.Verify(key, enrollTranscript(hubID, local, nonce, signedAt, hex.EncodeToString(csrHash[:])), sig) {
		return Hub{}, ErrDenied
	}
	_ = consumed
	b.Key = key
	return b, nil
}
func (s Store) EnrollmentReplay(ctx context.Context, hub, nonce string, csrHash []byte) (CertificateReplay, bool, error) {
	n, e := decode64(nonce, 32)
	if e != nil {
		return CertificateReplay{}, false, ErrDenied
	}
	hash := sha256.Sum256(n)
	var v CertificateReplay
	var oldHash []byte
	var consumed *time.Time
	var serial *string
	var certPEM *string
	var certExpiry *time.Time
	var certEpoch *int64
	var current int64
	var home *string
	var revoked bool
	e = s.Pool.QueryRow(ctx, `SELECT n.consumed_at,n.csr_hash,n.cert_serial,c.certificate_pem,c.expires_at,c.ownership_epoch,c.revoked_at IS NOT NULL,b.ownership_epoch,b.home_id FROM hub_enrollment_nonces n JOIN hubs b ON b.id=n.hub_id LEFT JOIN hub_certificates c ON c.serial=n.cert_serial WHERE n.nonce_hash=$1 AND b.id=$2 AND n.expires_at>now() AND b.revoked_at IS NULL`, hash[:], hub).Scan(&consumed, &oldHash, &serial, &certPEM, &certExpiry, &certEpoch, &revoked, &current, &home)
	if errors.Is(e, pgx.ErrNoRows) {
		return CertificateReplay{}, false, nil
	}
	if e != nil {
		return CertificateReplay{}, false, e
	}
	if consumed == nil {
		return CertificateReplay{}, false, nil
	}
	if !bytes.Equal(oldHash, csrHash) {
		return CertificateReplay{}, true, ErrKeyReuse
	}
	if serial == nil || certPEM == nil || certExpiry == nil || certEpoch == nil {
		return CertificateReplay{}, true, ErrDenied
	}
	v.PEM = *certPEM
	v.Expiry = *certExpiry
	v.Epoch = *certEpoch
	if revoked || !time.Now().Before(v.Expiry) || !(v.Epoch == current || (home != nil && v.Epoch == current-1)) {
		return CertificateReplay{}, true, ErrDenied
	}
	return v, true, nil
}
func (s Store) SaveEnrollmentCertificate(ctx context.Context, nonce, serial, hub string, epoch int64, expiry time.Time, pem string, csrHash []byte) error {
	n, e := decode64(nonce, 32)
	if e != nil {
		return ErrDenied
	}
	hash := sha256.Sum256(n)
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var current int64
	var exp time.Time
	var consumed *time.Time
	e = tx.QueryRow(ctx, `SELECT b.ownership_epoch,n.expires_at,n.consumed_at FROM hubs b JOIN hub_enrollment_nonces n ON n.hub_id=b.id AND n.nonce_hash=$2 WHERE b.id=$1 AND b.revoked_at IS NULL FOR UPDATE OF b,n`, hub, hash[:]).Scan(&current, &exp, &consumed)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrDenied
	}
	if e != nil {
		return e
	}
	if consumed != nil || !time.Now().Before(exp) || current != epoch {
		return ErrDenied
	}
	_, e = tx.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,$3,$4,$5)`, serial, hub, epoch, expiry, pem)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE hub_enrollment_nonces SET consumed_at=now(),csr_hash=$2,cert_serial=$3 WHERE nonce_hash=$1`, hash[:], csrHash, serial)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s Store) SaveRenewedCertificate(ctx context.Context, oldSerial, serial, hub string, epoch int64, expiry time.Time, pem string, csrHash []byte) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var current, oldEpoch int64
	var revoked bool
	var oldExpiry time.Time
	e = tx.QueryRow(ctx, `SELECT b.ownership_epoch,c.ownership_epoch,c.revoked_at IS NOT NULL,c.expires_at FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 WHERE b.id=$1 AND b.revoked_at IS NULL FOR UPDATE OF b,c`, hub, oldSerial).Scan(&current, &oldEpoch, &revoked, &oldExpiry)
	if e != nil || revoked || !time.Now().Before(oldExpiry) || current != epoch || !(oldEpoch == current || oldEpoch == current-1) {
		return ErrDenied
	}
	_, e = tx.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,$3,$4,$5)`, serial, hub, epoch, expiry, pem)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO hub_certificate_renewals(old_serial,csr_hash,new_serial) VALUES($1,$2,$3)`, oldSerial, csrHash, serial)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE hub_certificates SET revoked_at=now() WHERE serial=$1`, oldSerial)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

type CertificateReplay struct {
	PEM    string
	Expiry time.Time
	Epoch  int64
}

func (s Store) RenewalReplay(ctx context.Context, hub, oldSerial string, peerEpoch, epoch int64, csrHash []byte) (CertificateReplay, bool, error) {
	var v CertificateReplay
	var oldHash []byte
	var revoked bool
	var current, oldEpoch int64
	e := s.Pool.QueryRow(ctx, `SELECT n.csr_hash,c.certificate_pem,c.expires_at,c.ownership_epoch,c.revoked_at IS NOT NULL,b.ownership_epoch,oldc.ownership_epoch FROM hub_certificate_renewals n JOIN hub_certificates c ON c.serial=n.new_serial JOIN hub_certificates oldc ON oldc.serial=n.old_serial JOIN hubs b ON b.id=c.hub_id AND b.id=oldc.hub_id WHERE n.old_serial=$1 AND b.id=$2 AND b.revoked_at IS NULL`, oldSerial, hub).Scan(&oldHash, &v.PEM, &v.Expiry, &v.Epoch, &revoked, &current, &oldEpoch)
	if errors.Is(e, pgx.ErrNoRows) {
		return CertificateReplay{}, false, nil
	}
	if e != nil {
		return CertificateReplay{}, false, e
	}
	if !bytes.Equal(oldHash, csrHash) {
		return CertificateReplay{}, true, ErrKeyReuse
	}
	if revoked || oldEpoch != peerEpoch || !time.Now().Before(v.Expiry) || current != epoch || v.Epoch != epoch {
		return CertificateReplay{}, true, ErrDenied
	}
	return v, true, nil
}
func (s Store) VerifyCertificate(ctx context.Context, hub, serial string, epoch int64) (Hub, error) {
	var b Hub
	var certEpoch int64
	var key []byte
	e := s.Pool.QueryRow(ctx, `SELECT b.id,b.local_hub_id,b.device_public_key,b.ownership_epoch,b.version,b.revoked_at IS NOT NULL,CASE WHEN h.deleted_at IS NULL AND u.deleted_at IS NULL THEN b.home_id ELSE NULL END,c.ownership_epoch FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id LEFT JOIN homes h ON h.id=b.home_id LEFT JOIN users u ON u.id=h.owner_user_id WHERE b.id=$1 AND c.serial=$2 AND c.revoked_at IS NULL AND c.expires_at>now()`, hub, serial).Scan(&b.ID, &b.LocalID, &key, &b.Epoch, &b.Version, &b.Revoked, &b.HomeID, &certEpoch)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return Hub{}, e
	}
	if errors.Is(e, pgx.ErrNoRows) || b.Revoked || certEpoch != epoch || !(epoch == b.Epoch || (b.HomeID != nil && epoch == b.Epoch-1)) {
		return Hub{}, ErrDenied
	}
	b.Key = key
	return b, nil
}
