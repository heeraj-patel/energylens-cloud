package programs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RefreshWorker struct {
	DB       *pgxpool.Pool
	Resolver ZIPResolver
}

// RunOnce claims at most one coalesced job. A timed-out worker is recovered
// by the next pass, and a failed resolver never leaves the private job running.
func (w RefreshWorker) RunOnce(ctx context.Context) (bool, error) {
	if w.DB == nil || w.Resolver == nil {
		return false, errors.New("refresh worker unconfigured")
	}
	_, err := w.DB.Exec(ctx, `UPDATE program_discovery_refresh_jobs SET state='failed',failure_code='worker_timeout',finished_at=now()
 WHERE state='running' AND started_at<now()-interval '2 minutes'`)
	if err != nil {
		return false, err
	}
	_, err = w.DB.Exec(ctx, `DELETE FROM program_discovery_refresh_jobs j WHERE (j.state='pending' OR j.finished_at<now()-interval '30 days') AND NOT EXISTS (SELECT 1 FROM program_discovery_refresh_requests r WHERE r.job_id=j.id)`)
	if err != nil {
		return false, err
	}
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	var country, zip, category string
	err = tx.QueryRow(ctx, `SELECT id,country_code,postal_code,category FROM program_discovery_refresh_jobs j
 WHERE state='pending' AND EXISTS(SELECT 1 FROM program_discovery_refresh_requests r WHERE r.job_id=j.id)
 ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &country, &zip, &category)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE program_discovery_refresh_jobs SET state='running',started_at=now() WHERE id=$1`, id); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
	outcome, runErr := (DiscoveryRunner{DB: w.DB, Resolver: w.Resolver}).Run(runCtx, DiscoveryRequest{CountryCode: country, PostalCode: zip, Category: category})
	cancel()
	// Use a fresh context so cancellation and deadline errors are terminalized.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finishCancel()
	state, code := outcome.State, ""
	if runErr != nil {
		state, code = "failed", "runner_failed"
	}
	if state == "" {
		state, code = "failed", "runner_failed"
	}
	var runID any
	if outcome.RunID != uuid.Nil {
		runID = outcome.RunID
	}
	_, err = w.DB.Exec(finishCtx, `UPDATE program_discovery_refresh_jobs SET state=$2,run_id=$3,finished_at=now(),failure_code=$4
 WHERE id=$1 AND state='running'`, id, state, runID, nullableText(code))
	return true, err
}
