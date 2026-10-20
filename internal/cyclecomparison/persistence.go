package cyclecomparison

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrIdempotencyConflict = errors.New("idempotency key reused for different cycle")
var ErrEvidenceUnavailable = errors.New("cycle evidence unavailable")

func (s Store) Save(ctx context.Context, owner, home, appliance, key string, in Request, now time.Time) (SavedEvidence, error) {
	var out SavedEvidence
	payload, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	hash := sha256.Sum256(payload)
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Serialize retries and corrections against the same owned home. The
	// evidence is committed only with the repeatable measurement snapshot.
	var owned bool
	err = tx.QueryRow(ctx, `SELECT true FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var priorHash []byte
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM dishwasher_cycle_evidence WHERE owner_user_id=$1 AND home_id=$2 AND idempotency_key=$3`, owner, home, key).Scan(&out.ID, &priorHash)
	if err == nil {
		if string(priorHash) != string(hash[:]) {
			return SavedEvidence{}, ErrIdempotencyConflict
		}
		out, err = loadSaved(ctx, tx, owner, home, appliance, out.ID, now)
		if err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	result, refs, err := compareTx(ctx, tx, owner, home, appliance, in, now)
	if err != nil {
		return out, err
	}
	if result.Status != "available" {
		return out, ErrEvidenceUnavailable
	}
	var revision int64
	var sensor, hub string
	var epoch int64
	err = tx.QueryRow(ctx, `SELECT a.version,t.sensor_id,s.hub_id,s.ownership_epoch FROM appliances a JOIN attachments t ON t.appliance_id=a.id AND t.home_id=a.home_id JOIN sensor_endpoints s ON s.id=t.sensor_id WHERE a.id=$1 AND a.home_id=$2 AND t.id=$3`, appliance, home, in.AttachmentID).Scan(&revision, &sensor, &hub, &epoch)
	if err != nil {
		return out, err
	}
	var publicationHash, reviewHash, sourceHash string
	err = tx.QueryRow(ctx, `SELECT publication_sha256,review_sha256,source_sha256 FROM active_reviewed_usage_facts WHERE id=$1`, in.FactID).Scan(&publicationHash, &reviewHash, &sourceHash)
	if err != nil {
		return out, err
	}
	refJSON, err := json.Marshal(refs)
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO dishwasher_cycle_evidence(owner_user_id,home_id,appliance_id,appliance_version,attachment_id,sensor_id,hub_id,ownership_epoch,start_at,end_at,dedicated_no_other_loads,fact_id,fact_publication_sha256,fact_review_sha256,fact_source_sha256,confirmed_test_basis,confirmed_cycle_definition,measured_kwh,reference_kwh_per_cycle,interval_refs,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) RETURNING id`, owner, home, appliance, revision, in.AttachmentID, sensor, hub, epoch, in.Start, in.End, in.FactID, publicationHash, reviewHash, sourceHash, in.ConfirmedTestBasis, in.ConfirmedCycleDefinition, *result.MeasuredKWh, *result.LabReferenceKWhPerCycle, refJSON, key, hash[:]).Scan(&out.ID)
	if err != nil {
		return out, err
	}
	out, err = loadSaved(ctx, tx, owner, home, appliance, out.ID, now)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func loadSaved(ctx context.Context, tx pgx.Tx, owner, home, appliance, id string, now time.Time) (SavedEvidence, error) {
	var out SavedEvidence
	var measured, reference float64
	var invalidation *string
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT id,home_id,appliance_id,appliance_version,attachment_id,start_at,end_at,fact_id,confirmed_test_basis,confirmed_cycle_definition,dedicated_no_other_loads,measured_kwh,reference_kwh_per_cycle,created_at,invalidation_reason,expires_at FROM dishwasher_cycle_evidence WHERE id=$1 AND owner_user_id=$2 AND home_id=$3 AND appliance_id=$4 AND EXISTS(SELECT 1 FROM homes WHERE id=$3 AND owner_user_id=$2 AND deleted_at IS NULL)`, id, owner, home, appliance).Scan(&out.ID, &out.HomeID, &out.ApplianceID, &out.ApplianceVersion, &out.AttachmentID, &out.Start, &out.End, &out.FactID, &out.ConfirmedTestBasis, &out.ConfirmedCycleDefinition, &out.DedicatedNoOtherLoads, &measured, &reference, &out.CreatedAt, &invalidation, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if !expires.After(now) {
		return SavedEvidence{}, ErrNotFound
	}
	out.Interpretation = interpretation
	if invalidation != nil {
		out.Status = "invalidated"
		out.Reason = *invalidation
	} else {
		out.Status = "available"
		out.MeasuredKWh = &measured
		out.LabReferenceKWhPerCycle = &reference
		difference := measured - reference
		out.DifferenceKWh = &difference
	}
	return out, nil
}

func (s Store) Get(ctx context.Context, owner, home, appliance, id string, now time.Time) (SavedEvidence, error) {
	var out SavedEvidence
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	out, err = loadSaved(ctx, tx, owner, home, appliance, id, now)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
