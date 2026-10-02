package app

import (
	"context"
	"log/slog"
	"time"

	"energylens.local/cloud/internal/identityscan"
	"energylens.local/cloud/internal/meteredrollup"
)

const (
	rollupInterval    = time.Minute
	rollupDeadline    = 30 * time.Second
	rollupBatchLimit  = 10
	rollupBucketLimit = 100
	rollupPurgeLimit  = 100
)

type rollupJob interface {
	RunOnce(context.Context, time.Time, int, int, int) (meteredrollup.Stats, error)
}

type identityScanJob interface {
	Run(context.Context, identityscan.Scanner, bool, int, time.Time) (int, error)
}

func nightlyScanDue(now time.Time, last string) bool {
	return now.UTC().Hour() == 3 && now.UTC().Format("2006-01-02") != last
}

// runIdentityScans responds to new identity outbox rows within one short poll
// interval. Only the 03:00 UTC pass includes due missing-source retries.
func runIdentityScans(ctx context.Context, job identityScanJob, scanner identityscan.Scanner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastNightly := ""
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		nightly := nightlyScanDue(now, lastNightly)
		if nightly {
			lastNightly = now.Format("2006-01-02")
		}
		runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		count, err := job.Run(runCtx, scanner, nightly, 10, now)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("identity scan pass failed", "error", err)
		} else if count > 0 {
			slog.Info("identity scan candidates queued for review", "count", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runMeteredRollups runs one bounded pass at startup, then at most one pass per
// tick. The caller must cancel ctx and wait for this function before closing
// the database pool.
func runMeteredRollups(ctx context.Context, job rollupJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, rollupDeadline)
		stats, err := job.RunOnce(runCtx, time.Now().UTC(), rollupBatchLimit, rollupBucketLimit, rollupPurgeLimit)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("metered rollup pass failed", "error", err)
		} else if err == nil {
			slog.Info("metered rollup pass complete", "batches", stats.BatchesScanned, "buckets", stats.BucketsRecomputed, "details_purged", stats.DetailRowsPurged, "summaries_purged", stats.SummariesPurged)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
