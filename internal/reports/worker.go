package reports

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDeliveryDisabled = errors.New("report delivery is disabled")

const reportLeaseDuration = time.Minute

// DeliveryProvider must deduplicate on requestID; a provider success followed
// by a database commit failure can otherwise produce a second email on retry.
type DeliveryProvider interface {
	SendReport(context.Context, string, []string, RenderedReport) error
}

type DeliveryWorker struct {
	DB       *pgxpool.Pool
	Provider DeliveryProvider
	Enabled  bool // false by default; the Cloud runtime requires an explicit flag
}

// DeliverOne claims at most one due request, validates it while the lease is
// acquired, commits the lease, and only then calls the provider without holding
// database locks. The stable request ID is the provider idempotency key.
func (w DeliveryWorker) DeliverOne(ctx context.Context, now time.Time) (bool, error) {
	if !w.Enabled {
		return false, ErrDeliveryDisabled
	}
	if w.DB == nil || w.Provider == nil {
		return false, errors.New("report delivery provider is not configured")
	}
	now = now.UTC()
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
	var attempts int
	err = tx.QueryRow(ctx, `SELECT d.id,d.home_id,d.preview_id,d.recipients,d.approved_snapshot_sha256,
		p.payload,p.expires_at,p.home_version,h.version,d.attempts
		FROM report_delivery_requests d
		JOIN report_previews p ON p.id=d.preview_id AND p.owner_user_id=d.owner_user_id AND p.home_id=d.home_id
		JOIN homes h ON h.id=d.home_id AND h.owner_user_id=d.owner_user_id AND h.deleted_at IS NULL
		WHERE (d.state='pending' AND d.next_attempt_at <= $1)
		   OR (d.state='leased' AND d.lease_expires_at <= $1)
		ORDER BY CASE WHEN d.state='pending' THEN d.next_attempt_at ELSE d.lease_expires_at END,d.created_at,d.id
		LIMIT 1 FOR UPDATE OF d,h,p SKIP LOCKED`, now).Scan(&id, &home, &previewID, &recipients, &approved, &raw, &expires, &previewVersion, &homeVersion, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	leaseToken := uuid.NewString()
	if _, err = tx.Exec(ctx, `UPDATE report_delivery_requests SET state='leased',lease_token=$2,lease_expires_at=$3 WHERE id=$1`, id, leaseToken, now.Add(reportLeaseDuration)); err != nil {
		return false, err
	}

	// Serialize dependency changes during final pre-send validation. These
	// locks are released when the lease transaction commits, before network I/O.
	for _, query := range []string{
		`SELECT a.id FROM report_preview_items i JOIN appliances a ON a.id=i.appliance_id AND a.home_id=$2
		 WHERE i.preview_id=$1 AND i.demo_device_id IS NULL ORDER BY a.id FOR SHARE OF a`,
		`SELECT e.id FROM report_preview_items i JOIN estimate_runs e ON e.id=i.estimate_run_id AND e.home_id=$2
		 WHERE i.preview_id=$1 ORDER BY e.id FOR UPDATE OF e`,
	} {
		rows, queryErr := tx.Query(ctx, query, previewID, home)
		if queryErr != nil {
			return false, queryErr
		}
		for rows.Next() {
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return false, queryErr
		}
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
	err = tx.QueryRow(ctx, dependencyStaleSQL, previewID, home, previewVersion).Scan(&stale)
	if err != nil {
		return false, err
	}
	if normalized, e := normalizeRecipients(recipients); e != nil || len(normalized) != len(recipients) {
		stale = true
	}
	if stale || previewVersion != homeVersion || !now.Before(expires) || digest != approved || preview.SnapshotSHA256 != digest {
		_, err = tx.Exec(ctx, `UPDATE report_delivery_requests SET state='canceled',last_error_code='preview_stale',lease_token=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_token=$2`, id, leaseToken)
		if err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	report, err := RenderApprovedHTML(preview, approved)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}

	providerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	providerErr := w.Provider.SendReport(providerCtx, id, recipients, report)
	cancel()
	if providerErr == nil {
		_, err = w.DB.Exec(ctx, `UPDATE report_delivery_requests SET state='sent',attempts=attempts+1,sent_at=$3,last_error_code=NULL,lease_token=NULL,lease_expires_at=NULL
			WHERE id=$1 AND state='leased' AND lease_token=$2`, id, leaseToken, now)
		if err != nil {
			return true, err
		}
		return true, nil
	}

	nextAttempt := now.Add(reportRetryDelay(attempts + 1))
	nextState := "pending"
	if attempts+1 >= 5 {
		nextState = "canceled"
	}
	_, err = w.DB.Exec(ctx, `UPDATE report_delivery_requests SET attempts=attempts+1,state=$3,
		next_attempt_at=$4,last_error_code='provider_failed',lease_token=NULL,lease_expires_at=NULL
		WHERE id=$1 AND state='leased' AND lease_token=$2`, id, leaseToken, nextState, nextAttempt)
	if err != nil {
		return true, err
	}
	return true, providerErr
}

func reportRetryDelay(failedAttempt int) time.Duration {
	switch failedAttempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}
