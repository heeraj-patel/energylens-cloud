package kasaruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }
type ReadSnapshot struct {
	Snapshot
	HubID          string    `json:"hub_id"`
	OwnershipEpoch int64     `json:"ownership_epoch"`
	HomeID         string    `json:"home_id"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

func (s Store) Accept(ctx context.Context, hub, serial string, epoch int64, upload Upload) ([]string, error) {
	if upload.OwnershipEpoch != epoch {
		return nil, ErrDenied
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var home string
	// The hub lock prevents a transfer during this upload; the certificate must
	// still be current when the transaction commits.
	err = tx.QueryRow(ctx, `SELECT b.home_id FROM hubs b
 JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=$3 AND c.revoked_at IS NULL AND c.expires_at>now()
 JOIN homes h ON h.id=b.home_id AND h.deleted_at IS NULL
 WHERE b.id=$1 AND b.ownership_epoch=$3 AND b.revoked_at IS NULL
 FOR UPDATE OF b FOR SHARE OF c,h`, hub, serial, epoch).Scan(&home)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDenied
	}
	if err != nil {
		return nil, err
	}
	accepted := make([]string, 0, len(upload.Snapshots))
	for _, v := range upload.Snapshots {
		payload, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(payload)
		var old []byte
		err = tx.QueryRow(ctx, `SELECT payload_hash FROM kasa_runtime_snapshots WHERE hub_id=$1 AND ownership_epoch=$2 AND snapshot_id=$3`, hub, epoch, v.SnapshotID).Scan(&old)
		if err == nil {
			if !bytes.Equal(old, hash[:]) {
				return nil, ErrConflict
			}
			accepted = append(accepted, v.SnapshotID)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var sensor string
		err = tx.QueryRow(ctx, `SELECT id FROM sensor_endpoints WHERE id=$1 AND home_id=$2 AND hub_id=$3 AND ownership_epoch=$4 AND archived_at IS NULL AND kind='plug' FOR SHARE`, v.SensorID, home, hub, epoch).Scan(&sensor)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReference
		}
		if err != nil {
			return nil, err
		}
		if v.AttachmentID != nil {
			var attachment string
			err = tx.QueryRow(ctx, `SELECT id FROM attachments WHERE id=$1 AND home_id=$2 AND sensor_id=$3 AND started_at<=$4 AND (ended_at IS NULL OR ended_at>=$5) FOR SHARE`, *v.AttachmentID, home, v.SensorID, *v.ReportedStart, *v.ReportedEnd).Scan(&attachment)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrReference
			}
			if err != nil {
				return nil, err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO kasa_runtime_snapshots
   (hub_id,ownership_epoch,snapshot_id,home_id,sensor_id,attachment_id,scope,window_label,native_unit,native_minutes,duration_seconds,reported_start,reported_end,device_region,device_clock_valid,observed_at,method,transport,firmware,quality,payload_hash)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			hub, epoch, v.SnapshotID, home, v.SensorID, v.AttachmentID, v.Scope, v.WindowLabel, v.NativeUnit, v.NativeMinutes, v.DurationSeconds, v.ReportedStart, v.ReportedEnd, v.DeviceRegion, v.DeviceClockValid, v.ObservedAt, v.Method, v.Transport, v.Firmware, v.Quality, hash[:])
		if err != nil {
			return nil, err
		}
		accepted = append(accepted, v.SnapshotID)
	}
	return accepted, tx.Commit(ctx)
}
func (s Store) Page(ctx context.Context, home, owner string, c pageCursor, limit int) ([]ReadSnapshot, error) {
	// Keep authorization in the row-producing statement. A transfer between a
	// separate owner check and this read must never expose the next owner's rows.
	rows, err := s.Pool.Query(ctx, `SELECT k.hub_id,k.ownership_epoch,k.snapshot_id,k.home_id,k.sensor_id,k.attachment_id,k.scope,k.window_label,k.native_unit,k.native_minutes,k.duration_seconds,k.reported_start,k.reported_end,k.device_region,k.device_clock_valid,k.observed_at,k.method,k.transport,k.firmware,k.quality,k.accepted_at
 FROM kasa_runtime_snapshots k JOIN homes h ON h.id=k.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL
 WHERE k.home_id=$1 AND k.observed_at >= $3 AND k.observed_at <= $4 AND
 ($5::timestamptz IS NULL OR (k.observed_at,k.snapshot_id) < ($5,$6::uuid))
 ORDER BY k.observed_at DESC,k.snapshot_id DESC LIMIT $7`, home, owner, c.From, c.To, func() any {
		if c.ID == "" {
			return nil
		}
		return c.At
	}(), func() any {
		if c.ID == "" {
			return nil
		}
		return c.ID
	}(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ReadSnapshot{}
	for rows.Next() {
		var v ReadSnapshot
		err = rows.Scan(&v.HubID, &v.OwnershipEpoch, &v.SnapshotID, &v.HomeID, &v.SensorID, &v.AttachmentID, &v.Scope, &v.WindowLabel, &v.NativeUnit, &v.NativeMinutes, &v.DurationSeconds, &v.ReportedStart, &v.ReportedEnd, &v.DeviceRegion, &v.DeviceClockValid, &v.ObservedAt, &v.Method, &v.Transport, &v.Firmware, &v.Quality, &v.AcceptedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(result) == 0 {
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, home, owner).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrReference
		}
	}
	return result, nil
}
