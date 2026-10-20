package datawizresolution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"energylens.local/cloud/internal/datawizrequests"
	"github.com/jackc/pgx/v5"
)

const refreshHomeDayLimit = 30

// RateLimited carries the wait until the next refresh is accepted.
type RateLimited struct{ RetryAfter time.Duration }

func (e RateLimited) Error() string { return "refresh rate limited" }

type RefreshResult struct {
	ApplianceID string    `json:"appliance_id"`
	Reason      string    `json:"reason"`
	QueuedAt    time.Time `json:"queued_at"`
}

// Refresh queues one owner re-resolution; a row already queued is returned as is.
func (s PGStore) Refresh(ctx context.Context, owner, home, appliance, key string) (RefreshResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, ErrNotFound
	}
	if err != nil {
		return RefreshResult{}, err
	}
	op := "refresh_energy_resolution:" + appliance
	hash := sha256.Sum256([]byte(op))
	var previous []byte
	var body json.RawMessage
	err = tx.QueryRow(ctx, `SELECT request_hash,response_json FROM appliance_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND operation=$3 AND key=$4`, owner, home, op, key).Scan(&previous, &body)
	if err == nil {
		var result RefreshResult
		return result, json.Unmarshal(body, &result)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM appliances WHERE id=$1 AND home_id=$2 AND archived_at IS NULL`, appliance, home).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, ErrNotFound
	}
	if err != nil {
		return RefreshResult{}, err
	}
	result := RefreshResult{ApplianceID: appliance, Reason: "owner_refresh"}
	err = tx.QueryRow(ctx, `SELECT queued_at FROM datawiz_resolve_queue WHERE appliance_id=$1`, appliance).Scan(&result.QueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = refreshLimit(ctx, tx, home, appliance); err != nil {
			return RefreshResult{}, err
		}
		result.QueuedAt, err = datawizrequests.QueueWithAttempt(ctx, tx, appliance, home, result.Reason)
	}
	if err != nil {
		return RefreshResult{}, err
	}
	body, err = json.Marshal(result)
	if err != nil {
		return RefreshResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO appliance_idempotency(owner_user_id,home_id,operation,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5,$6)`, owner, home, op, key, hash[:], body); err != nil {
		return RefreshResult{}, err
	}
	return result, tx.Commit(ctx)
}

// refreshLimit allows 1 accepted refresh per appliance per 10 minutes and 30 per home per day.
func refreshLimit(ctx context.Context, tx pgx.Tx, home, appliance string) error {
	var wait float64
	err := tx.QueryRow(ctx, `SELECT COALESCE(max(extract(epoch FROM queued_at+interval '10 minutes'-clock_timestamp())),0) FROM datawiz_resolve_attempts WHERE appliance_id=$1 AND reason='owner_refresh' AND queued_at>clock_timestamp()-interval '10 minutes'`, appliance).Scan(&wait)
	if err != nil {
		return err
	}
	if wait > 0 {
		return RateLimited{time.Duration(wait * float64(time.Second))}
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(extract(epoch FROM min(queued_at)+interval '1 day'-clock_timestamp()),0) FROM (SELECT queued_at FROM datawiz_resolve_attempts WHERE home_id=$1 AND reason='owner_refresh' AND queued_at>clock_timestamp()-interval '1 day' ORDER BY queued_at DESC LIMIT $2) recent HAVING count(*)>=$2`, home, refreshHomeDayLimit).Scan(&wait)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return RateLimited{time.Duration(wait * float64(time.Second))}
}
