package reports

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDeliveryDisabled = errors.New("report delivery is disabled")

// DeliveryProvider must deduplicate on requestID; a provider success followed
// by a database commit failure can otherwise produce a second email on retry.
type DeliveryProvider interface {
	SendReport(context.Context, string, []string, Preview) error
}

type DeliveryWorker struct {
	DB       *pgxpool.Pool
	Provider DeliveryProvider
	Enabled  bool // false by default; no app role schedules this worker
}

// DeliverOne processes at most one manually queued request. It checks the
// snapshot and dependencies again immediately before calling the provider.
func (w DeliveryWorker) DeliverOne(ctx context.Context, now time.Time) (bool, error) {
	if !w.Enabled {
		return false, ErrDeliveryDisabled
	}
	if w.DB == nil || w.Provider == nil {
		return false, errors.New("report delivery provider is not configured")
	}
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, home, previewID string
	var recipients []string
	var approved string
	var raw []byte
	var expires time.Time
	var previewVersion, homeVersion int64
	err = tx.QueryRow(ctx, `SELECT d.id,d.home_id,d.preview_id,d.recipients,d.approved_snapshot_sha256,
		p.payload,p.expires_at,p.home_version,h.version
		FROM report_delivery_requests d
		JOIN report_previews p ON p.id=d.preview_id AND p.owner_user_id=d.owner_user_id AND p.home_id=d.home_id
		JOIN homes h ON h.id=d.home_id AND h.owner_user_id=d.owner_user_id AND h.deleted_at IS NULL
		WHERE d.state='pending' ORDER BY d.created_at,d.id LIMIT 1 FOR UPDATE OF d,h SKIP LOCKED`).Scan(&id, &home, &previewID, &recipients, &approved, &raw, &expires, &previewVersion, &homeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var preview Preview
	if err = json.Unmarshal(raw, &preview); err != nil {
		return false, err
	}
	digest, err := snapshotDigest(preview)
	if err != nil {
		return false, err
	}
	var stale bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM report_preview_items i LEFT JOIN appliances a ON a.id=i.appliance_id AND a.home_id=$2
		WHERE i.preview_id=$1 AND (a.id IS NULL OR a.archived_at IS NOT NULL OR a.version<>i.appliance_version OR
		(i.estimate_run_id IS NOT NULL AND EXISTS(SELECT 1 FROM estimate_run_invalidations v WHERE v.run_id=i.estimate_run_id)))
	)`, previewID, home).Scan(&stale)
	if err != nil {
		return false, err
	}
	if normalized, e := normalizeRecipients(recipients); e != nil || len(normalized) != len(recipients) {
		stale = true
	}
	if stale || previewVersion != homeVersion || !now.Before(expires) || digest != approved || preview.SnapshotSHA256 != digest {
		_, err = tx.Exec(ctx, `UPDATE report_delivery_requests SET state='canceled',last_error_code='preview_stale' WHERE id=$1`, id)
		if err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	providerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = w.Provider.SendReport(providerCtx, id, recipients, preview)
	cancel()
	if err != nil {
		_, updateErr := tx.Exec(ctx, `UPDATE report_delivery_requests SET attempts=attempts+1,
			state=CASE WHEN attempts+1>=5 THEN 'canceled' ELSE 'pending' END,
			last_error_code='provider_failed' WHERE id=$1`, id)
		if updateErr != nil {
			return false, updateErr
		}
		if updateErr = tx.Commit(ctx); updateErr != nil {
			return false, updateErr
		}
		return true, err
	}
	_, err = tx.Exec(ctx, `UPDATE report_delivery_requests SET state='sent',attempts=attempts+1,sent_at=$2,last_error_code=NULL WHERE id=$1`, id, now.UTC())
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
