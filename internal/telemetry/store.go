package telemetry

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

// VerifyCurrent rejects revoked and prior-epoch certificates before decoding a payload.
// Accept repeats the check while holding the hub lock through commit.
func (s Store) VerifyCurrent(ctx context.Context, hub, serial string, epoch int64) error {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=$3 AND c.revoked_at IS NULL AND c.expires_at>now() JOIN homes h ON h.id=b.home_id AND h.deleted_at IS NULL WHERE b.id=$1 AND b.ownership_epoch=$3 AND b.revoked_at IS NULL)`, hub, serial, epoch).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrDenied
	}
	return nil
}
func (s Store) Accept(ctx context.Context, hub, serial string, epoch int64, b Batch, hash []byte) (Acknowledgement, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Acknowledgement{}, err
	}
	defer tx.Rollback(ctx)
	var home string
	// Hold the current association stable until the batch commits.
	err = tx.QueryRow(ctx, `SELECT b.home_id FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=$3 AND c.revoked_at IS NULL AND c.expires_at>now() JOIN homes h ON h.id=b.home_id AND h.deleted_at IS NULL WHERE b.id=$1 AND b.ownership_epoch=$3 AND b.revoked_at IS NULL FOR UPDATE OF b FOR SHARE OF c,h`, hub, serial, epoch).Scan(&home)
	if errors.Is(err, pgx.ErrNoRows) {
		return Acknowledgement{}, ErrDenied
	}
	if err != nil {
		return Acknowledgement{}, err
	}
	var oldHash, oldResponse []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_json FROM ingest_batches WHERE hub_id=$1 AND ownership_epoch=$2 AND batch_id=$3`, hub, epoch, b.BatchID).Scan(&oldHash, &oldResponse)
	if err == nil {
		if !bytes.Equal(oldHash, hash) {
			return Acknowledgement{}, ErrConflict
		}
		var ack Acknowledgement
		if err = json.Unmarshal(oldResponse, &ack); err != nil {
			return Acknowledgement{}, err
		}
		return ack, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Acknowledgement{}, err
	}
	var ack Acknowledgement
	ack.BatchID = b.BatchID
	ack.OwnershipEpoch = epoch
	ack.AcceptedEventIDs = []string{}
	err = tx.QueryRow(ctx, `INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,$2,$3,$4,$5) RETURNING cursor,accepted_at`, hub, epoch, b.BatchID, home, hash).Scan(&ack.Cursor, &ack.AcknowledgedAt)
	if err != nil {
		return Acknowledgement{}, err
	}
	for _, event := range b.Events {
		if err = s.acceptEvent(ctx, tx, hub, home, epoch, b.BatchID, event); err != nil {
			return Acknowledgement{}, err
		}
		ack.AcceptedEventIDs = append(ack.AcceptedEventIDs, event.id())
	}
	body, err := json.Marshal(ack)
	if err != nil {
		return Acknowledgement{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE ingest_batches SET response_json=$4 WHERE hub_id=$1 AND ownership_epoch=$2 AND batch_id=$3`, hub, epoch, b.BatchID, body)
	if err != nil {
		return Acknowledgement{}, err
	}
	return ack, tx.Commit(ctx)
}
func (s Store) acceptEvent(ctx context.Context, tx pgx.Tx, hub, home string, epoch int64, batchID string, e Event) error {
	var sensor string
	err := tx.QueryRow(ctx, `SELECT id FROM sensor_endpoints WHERE id=$1 AND home_id=$2 AND hub_id=$3 AND ownership_epoch=$4 AND archived_at IS NULL FOR SHARE`, e.SensorID, home, hub, epoch).Scan(&sensor)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReference
	}
	if err != nil {
		return err
	}
	if e.Kind == "energy_interval" {
		var id string
		err = tx.QueryRow(ctx, `SELECT a.id FROM attachments a JOIN sensor_endpoints s ON s.id=a.sensor_id AND s.home_id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND a.sensor_id=$3 AND s.hub_id=$4 AND s.ownership_epoch=$5 AND a.started_at<=$6 AND (a.ended_at IS NULL OR a.ended_at>=$7)`, *e.AttachmentRevisionID, home, e.SensorID, hub, epoch, *e.Start, *e.End).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReference
		}
		if err != nil {
			return err
		}
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	var oldHash []byte
	err = tx.QueryRow(ctx, `SELECT payload_hash FROM ingest_events WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND revision=$4`, hub, epoch, e.id(), e.Revision).Scan(&oldHash)
	if err == nil {
		if !bytes.Equal(oldHash, sum[:]) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if e.Revision > 1 {
		var prior time.Time
		var priorSensor string
		err = tx.QueryRow(ctx, `SELECT accepted_at,sensor_id FROM ingest_events WHERE hub_id=$1 AND ownership_epoch=$2 AND event_id=$3 AND revision=$4 AND event_kind=$5`, hub, epoch, e.id(), e.Revision-1, e.Kind).Scan(&prior, &priorSensor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if time.Since(prior) > 30*24*time.Hour || priorSensor != e.SensorID {
			return ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, hub, epoch, e.id(), e.Revision, home, e.SensorID, e.Kind, sum[:], batchID)
	if err != nil {
		return err
	}
	if e.Kind == "coverage_gap" {
		_, err = tx.Exec(ctx, `INSERT INTO coverage_gaps(hub_id,ownership_epoch,event_id,revision,first_at,last_at,reason,dropped_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, hub, epoch, e.id(), e.Revision, *e.FirstTimestamp, *e.LastTimestamp, *e.Reason, *e.DroppedCount)
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,observed_at,start_at,end_at,watts,kwh_cumulative,energy_wh,covered_seconds,attachment_revision_id,fuel,quality,source,source_method,source_transport) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, hub, epoch, e.id(), e.Revision, e.Kind, e.Timestamp, e.Start, e.End, e.Watts, e.KWhCumulative, e.EnergyWh, e.CoveredSeconds, e.AttachmentRevisionID, e.Fuel, e.Quality, e.Source, e.SourceMethod, e.SourceTransport)
	return err
}
