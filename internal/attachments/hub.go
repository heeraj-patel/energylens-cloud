package attachments

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const maxHubAttachments = 4096

type HubFeed struct {
	HubID          string       `json:"hub_id"`
	HomeID         string       `json:"home_id"`
	OwnershipEpoch int64        `json:"ownership_epoch"`
	Attachments    []Attachment `json:"attachments"`
}

var ErrHubDenied = errors.New("hub certificate or ownership epoch denied")
var ErrHubFeedTooLarge = errors.New("hub attachment feed exceeds limit")

func (s Service) HubRoutes(r chi.Router) { r.Get("/hubs/{hub}/attachments", s.listHub) }

func (s Service) listHub(w http.ResponseWriter, r *http.Request) {
	hub := chi.URLParam(r, "hub")
	if !validUUID(hub) {
		fail(w, 404, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "forbidden")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	peerHub, epoch, ok := hubtrust.PeerIdentity(peer)
	if !ok || peerHub != hub {
		fail(w, 403, "forbidden")
		return
	}
	feed, err := s.Store.ListHub(r.Context(), hub, peer.SerialNumber.Text(16), epoch)
	switch {
	case errors.Is(err, ErrHubDenied):
		fail(w, 403, "forbidden")
	case errors.Is(err, ErrHubFeedTooLarge):
		fail(w, 409, "attachment_feed_too_large")
	case err != nil:
		slog.Error("hub attachment feed failed", "error", err)
		fail(w, 500, "internal_error")
	default:
		write(w, 200, feed)
	}
}

func (s Store) ListHub(ctx context.Context, hub, serial string, peerEpoch int64) (HubFeed, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return HubFeed{}, err
	}
	defer tx.Rollback(ctx)
	var feed HubFeed
	err = tx.QueryRow(ctx, `SELECT b.id,b.home_id,b.ownership_epoch FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=$3 AND c.revoked_at IS NULL AND c.expires_at>now() WHERE b.id=$1 AND b.home_id IS NOT NULL AND b.ownership_epoch=$3 AND b.revoked_at IS NULL FOR SHARE OF b,c`, hub, serial, peerEpoch).Scan(&feed.HubID, &feed.HomeID, &feed.OwnershipEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return HubFeed{}, ErrHubDenied
	}
	if err != nil {
		return HubFeed{}, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id,a.home_id,a.sensor_id,a.appliance_id,a.identity_revision,a.started_at,a.ended_at,a.version,a.created_at,a.updated_at FROM attachments a JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id WHERE a.home_id=$1 AND s.hub_id=$2 AND s.ownership_epoch=$3 ORDER BY a.started_at,a.id LIMIT $4`, feed.HomeID, hub, peerEpoch, maxHubAttachments+1)
	if err != nil {
		return HubFeed{}, err
	}
	feed.Attachments = []Attachment{}
	for rows.Next() {
		a, e := scan(rows)
		if e != nil {
			rows.Close()
			return HubFeed{}, e
		}
		feed.Attachments = append(feed.Attachments, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return HubFeed{}, err
	}
	if len(feed.Attachments) > maxHubAttachments {
		return HubFeed{}, ErrHubFeedTooLarge
	}
	return feed, tx.Commit(ctx)
}
