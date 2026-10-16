package hubtrust

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

var connectionPin = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Connection struct {
	HTTPSOrigin       string    `json:"https_origin"`
	CertificateSHA256 string    `json:"certificate_sha256"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func validConnection(origin, pin string) bool {
	if len(origin) > 128 || !connectionPin.MatchString(pin) {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.String() != origin {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535
}

// PublishConnection accepts a discovery hint only from the Hub's current,
// unrevoked mTLS certificate at a claimed home and epoch. It grants no access.
func (s Store) PublishConnection(ctx context.Context, hubID, serial string, peerEpoch int64, origin, pin string) error {
	if !validConnection(origin, pin) || peerEpoch < 2 {
		return ErrDenied
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var homeID string
	var certExpiry time.Time
	err = tx.QueryRow(ctx, `SELECT b.home_id,c.expires_at FROM hubs b
		JOIN homes h ON h.id=b.home_id AND h.deleted_at IS NULL
		JOIN users u ON u.id=h.owner_user_id AND u.deleted_at IS NULL
		JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=b.ownership_epoch
			AND c.revoked_at IS NULL AND c.expires_at>now()
		WHERE b.id=$1 AND b.ownership_epoch=$3 AND b.ownership_epoch>=2
			AND b.home_id IS NOT NULL AND b.revoked_at IS NULL
		FOR UPDATE OF b`, hubID, serial, peerEpoch).Scan(&homeID, &certExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO hub_connections
		(hub_id,home_id,ownership_epoch,publisher_cert_serial,https_origin,certificate_sha256,published_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,now(),LEAST(now()+interval '10 minutes',$7))
		ON CONFLICT(hub_id) DO UPDATE SET home_id=excluded.home_id,
		ownership_epoch=excluded.ownership_epoch,publisher_cert_serial=excluded.publisher_cert_serial,
		https_origin=excluded.https_origin,certificate_sha256=excluded.certificate_sha256,
		published_at=excluded.published_at,expires_at=excluded.expires_at`, hubID, homeID, peerEpoch, serial, origin, pin, certExpiry)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
