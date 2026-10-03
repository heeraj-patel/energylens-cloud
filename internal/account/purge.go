package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrPurgeNotReady = errors.New("deletion tombstone or hub revocation missing")
var ErrPurgeArchiveNotReady = errors.New("deletion journal archive is not ready for purge")

// PurgeHome is an operator-only method; no API route invokes it. It physically
// removes the private rows for an already tombstoned home, then leaves a
// minimal disabled home row and the deletion journal for replay/denial.
func (s PGStore) PurgeHome(ctx context.Context, home string, archive JournalArchive) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyPurgeArchive(ctx, tx, archive); err != nil {
		return err
	}
	if err = purgeHomeTx(ctx, tx, home); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PurgeAccount removes account preferences and request payloads after every
// owned home has been tombstoned and purged. The user ID, disabled identity
// mapping, and deletion journal remain so the same OIDC subject cannot silently
// create a new account after deletion.
func (s PGStore) PurgeAccount(ctx context.Context, owner string, archive JournalArchive) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyPurgeArchive(ctx, tx, archive); err != nil {
		return err
	}
	var deleted bool
	err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL AND EXISTS (SELECT 1 FROM privacy_deletion_journal WHERE kind='account' AND object_id=$1) FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !deleted {
		return ErrPurgeNotReady
	}
	rows, err := tx.Query(ctx, `SELECT id FROM homes WHERE owner_user_id=$1 ORDER BY id`, owner)
	if err != nil {
		return err
	}
	homes := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		homes = append(homes, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, home := range homes {
		if err = purgeHomeTx(ctx, tx, home); err != nil {
			return err
		}
	}
	for _, sql := range []string{
		`DELETE FROM hub_request_idempotency WHERE owner_user_id=$1`,
		`DELETE FROM account_home_idempotency WHERE owner_user_id=$1`,
		`DELETE FROM preference_idempotency WHERE owner_user_id=$1`,
		`DELETE FROM account_preference_revisions WHERE user_id=$1`,
		`DELETE FROM account_preferences WHERE user_id=$1`,
	} {
		if _, err = tx.Exec(ctx, sql, owner); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE users SET email=NULL,email_verified=false WHERE id=$1`, owner)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// verifyPurgeArchive compares the authenticated archive with the complete
// database journal in one transaction snapshot. Older archive-only entries are
// valid after a database restore; a missing or changed current entry is not.
func verifyPurgeArchive(ctx context.Context, tx pgx.Tx, archive JournalArchive) error {
	if archive == nil {
		return ErrPurgeArchiveNotReady
	}
	entries, err := archive.ReadAll()
	if err != nil {
		return fmt.Errorf("read deletion journal archive: %w", err)
	}
	bySequence := make(map[int64]DeletionEntry, len(entries))
	for _, e := range entries {
		if err := validDeletionEntry(e); err != nil {
			return fmt.Errorf("invalid archived deletion entry: %w", err)
		}
		if _, exists := bySequence[e.Sequence]; exists {
			return fmt.Errorf("duplicate archived deletion sequence %d: %w", e.Sequence, ErrPurgeArchiveNotReady)
		}
		bySequence[e.Sequence] = e
	}
	rows, err := tx.Query(ctx, `SELECT sequence,kind,object_id,deleted_at FROM privacy_deletion_journal ORDER BY sequence`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var current DeletionEntry
		if err := rows.Scan(&current.Sequence, &current.Kind, &current.ObjectID, &current.DeletedAt); err != nil {
			return err
		}
		archived, ok := bySequence[current.Sequence]
		if !ok || archived.Kind != current.Kind || archived.ObjectID != current.ObjectID || !archived.DeletedAt.Equal(current.DeletedAt) {
			return fmt.Errorf("deletion sequence %d: %w", current.Sequence, ErrPurgeArchiveNotReady)
		}
	}
	return rows.Err()
}

func purgeHomeTx(ctx context.Context, tx pgx.Tx, home string) error {
	var owner string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT owner_user_id,deleted_at IS NOT NULL AND EXISTS (SELECT 1 FROM privacy_deletion_journal WHERE kind='home' AND object_id=$1) FROM homes WHERE id=$1 FOR UPDATE`, home).Scan(&owner, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !deleted {
		return ErrPurgeNotReady
	}
	var active int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM hubs WHERE home_id=$1`, home).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return ErrPurgeNotReady
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM hub_certificates c JOIN hub_ownership_history o ON o.hub_id=c.hub_id AND o.ownership_epoch=c.ownership_epoch WHERE o.home_id=$1 AND c.revoked_at IS NULL`, home).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return ErrPurgeNotReady
	}
	// Delete leaf references before their parents. Keep shared/public reviewed
	// catalog, utility programs, and coarse weather caches untouched.
	for _, sql := range []string{
		`DELETE FROM program_discovery_refresh_requests WHERE home_id=$1`,
		`DELETE FROM report_delivery_requests WHERE home_id=$1`,
		`DELETE FROM report_preview_items WHERE preview_id IN (SELECT id FROM report_previews WHERE home_id=$1)`,
		`DELETE FROM report_previews WHERE home_id=$1`,
		`DELETE FROM estimate_run_invalidations WHERE run_id IN (SELECT id FROM estimate_runs WHERE home_id=$1)`,
		`DELETE FROM estimate_runs WHERE home_id=$1`,
		`DELETE FROM question_answers WHERE home_id=$1`,
		`DELETE FROM tariff_revisions WHERE home_id=$1`,
		`DELETE FROM kasa_runtime_snapshots WHERE home_id=$1`,
		`DELETE FROM energy_hourly WHERE home_id=$1`,
		`DELETE FROM energy_daily WHERE home_id=$1`,
		`DELETE FROM power_hourly WHERE home_id=$1`,
		`DELETE FROM power_daily WHERE home_id=$1`,
		`DELETE FROM coverage_hourly WHERE home_id=$1`,
		`DELETE FROM coverage_daily WHERE home_id=$1`,
		`DELETE FROM metered_rollup_dirty WHERE home_id=$1`,
		`DELETE FROM energy_observations WHERE (hub_id,ownership_epoch,event_id,revision) IN (SELECT hub_id,ownership_epoch,event_id,revision FROM ingest_events WHERE home_id=$1)`,
		`DELETE FROM coverage_gaps WHERE (hub_id,ownership_epoch,event_id,revision) IN (SELECT hub_id,ownership_epoch,event_id,revision FROM ingest_events WHERE home_id=$1)`,
		`DELETE FROM ingest_events WHERE home_id=$1`,
		`DELETE FROM ingest_batches WHERE home_id=$1`,
		`DELETE FROM question_answer_idempotency WHERE home_id=$1`,
		`DELETE FROM tariff_idempotency WHERE home_id=$1`,
		`DELETE FROM intervention_idempotency WHERE home_id=$1`,
		`DELETE FROM interventions WHERE home_id=$1`,
		`DELETE FROM attachment_request_idempotency WHERE home_id=$1`,
		`DELETE FROM appliance_idempotency WHERE home_id=$1`,
		`DELETE FROM sensor_request_idempotency WHERE home_id=$1`,
		`DELETE FROM attachment_revisions WHERE home_id=$1`,
		`DELETE FROM attachments WHERE home_id=$1`,
		`UPDATE sensor_endpoints SET parent_sensor_id=NULL WHERE home_id=$1 AND parent_sensor_id IS NOT NULL`,
		`DELETE FROM sensor_endpoints WHERE home_id=$1`,
		`DELETE FROM appliance_revisions WHERE home_id=$1`,
		`DELETE FROM identity_scan_requests WHERE home_id=$1`,
		`DELETE FROM appliances WHERE home_id=$1`,
		`DELETE FROM home_location_revisions WHERE home_id=$1`,
		`DELETE FROM home_preference_revisions WHERE home_id=$1`,
		`DELETE FROM home_preferences WHERE home_id=$1`,
		`DELETE FROM preference_idempotency WHERE owner_user_id=(SELECT owner_user_id FROM homes WHERE id=$1) AND operation='home_preferences:'||$1::text`,
		`DELETE FROM account_home_idempotency WHERE owner_user_id=(SELECT owner_user_id FROM homes WHERE id=$1) AND (operation='patch_home:'||$1::text OR response_json->>'id'=$1::text)`,
		`DELETE FROM hub_request_idempotency WHERE owner_user_id=(SELECT owner_user_id FROM homes WHERE id=$1) AND (response_json->>'home_id'=$1::text OR response_json->>'claim_id' IN (SELECT id::text FROM hub_claims WHERE home_id=$1))`,
		`DELETE FROM hub_possession_proofs WHERE claim_id IN (SELECT id FROM hub_claims WHERE home_id=$1)`,
		`DELETE FROM hub_claims WHERE home_id=$1`,
		`DELETE FROM hub_certificate_renewals WHERE old_serial IN (SELECT c.serial FROM hub_certificates c JOIN hub_ownership_history o ON o.hub_id=c.hub_id AND o.ownership_epoch=c.ownership_epoch WHERE o.home_id=$1) OR new_serial IN (SELECT c.serial FROM hub_certificates c JOIN hub_ownership_history o ON o.hub_id=c.hub_id AND o.ownership_epoch=c.ownership_epoch WHERE o.home_id=$1)`,
		`DELETE FROM hub_enrollment_nonces WHERE cert_serial IN (SELECT c.serial FROM hub_certificates c JOIN hub_ownership_history o ON o.hub_id=c.hub_id AND o.ownership_epoch=c.ownership_epoch WHERE o.home_id=$1)`,
		`DELETE FROM hub_certificates WHERE (hub_id,ownership_epoch) IN (SELECT hub_id,ownership_epoch FROM hub_ownership_history WHERE home_id=$1)`,
		`DELETE FROM hub_ownership_history WHERE home_id=$1`,
	} {
		if _, err = tx.Exec(ctx, sql, home); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE homes SET name='Deleted home',timezone='UTC' WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NOT NULL`, home, owner)
	return err
}
