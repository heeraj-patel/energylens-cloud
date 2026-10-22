package app

import (
	"context"
	"log/slog"
	"time"

	"energylens.local/cloud/internal/meteredrollup"
)

const (
	rollupInterval               = time.Minute
	rollupDeadline               = 30 * time.Second
	rollupBatchLimit             = 10
	rollupBucketLimit            = 100
	rollupPurgeLimit             = 100
	reportRetentionInterval      = time.Hour
	reportRetentionDeadline      = 30 * time.Second
	reportRetentionLimit         = 100
	kasaRuntimeRetentionInterval = time.Minute
	kasaRuntimeRetentionDeadline = 30 * time.Second
	kasaRuntimeRetentionLimit    = 500
	reportDeliveryInterval       = 10 * time.Second
	reportDeliveryDeadline       = 20 * time.Second
)

type rollupJob interface {
	RunOnce(context.Context, time.Time, int, int, int) (meteredrollup.Stats, error)
}

type reportRetentionJob interface {
	RunOnce(context.Context, time.Time, int) (int64, error)
}

type kasaRuntimeRetentionJob interface {
	RunOnce(context.Context, time.Time, int) (int64, error)
}

func runKasaRuntimeRetention(ctx context.Context, job kasaRuntimeRetentionJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, kasaRuntimeRetentionDeadline)
		deleted, err := job.RunOnce(runCtx, time.Now().UTC(), kasaRuntimeRetentionLimit)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("Kasa runtime retention pass failed", "error", err)
		} else if deleted > 0 {
			slog.Info("Kasa runtime retention pass complete", "snapshots_deleted", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type reportDeliveryJob interface {
	DeliverOne(context.Context, time.Time) (bool, error)
}

// runReportDeliveries only consumes manually approved pending requests. A
// pass processes at most one row and cannot start another before it finishes.
func runReportDeliveries(ctx context.Context, job reportDeliveryJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, reportDeliveryDeadline)
		_, err := job.DeliverOne(runCtx, time.Now().UTC())
		cancel()
		if err != nil && ctx.Err() == nil {
			// Provider/SQL errors may carry private content; keep routine logs redacted.
			slog.Error("report delivery pass failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runReportRetention(ctx context.Context, job reportRetentionJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, reportRetentionDeadline)
		deleted, err := job.RunOnce(runCtx, time.Now().UTC(), reportRetentionLimit)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("report retention pass failed", "error", err)
		} else if deleted > 0 {
			slog.Info("report retention pass complete", "snapshots_deleted", deleted)
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

type programRefreshJob interface {
	RunOnce(context.Context) (bool, error)
}

type datawizRequestJob interface {
	RunOnce(context.Context) (int, error)
}

func runDatawizRequests(ctx context.Context, job datawizRequestJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		count, err := job.RunOnce(runCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("Datawiz request pass failed", "error", err)
		} else if count > 0 {
			slog.Info("Datawiz job queued or wrote requests", "count", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runProgramRefreshes(ctx context.Context, job programRefreshJob, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, err := job.RunOnce(runCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("program refresh pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

const (
	ratePlanChangeInterval = 5 * time.Minute
	ratePlanSweepInterval  = 24 * time.Hour
	ratePlanSweepDeadline  = 10 * time.Minute
)

type ratePlanSweepJob interface {
	RunOnce(context.Context, time.Time, *time.Time) (int, error)
}

// runRatePlanSweeps runs the full default and refresh sweep daily (and at start), and a pass over moved homes in between.
func runRatePlanSweeps(ctx context.Context, job ratePlanSweepJob, changeInterval, fullInterval time.Duration) {
	ticker := time.NewTicker(changeInterval)
	defer ticker.Stop()
	var lastFull, lastPass time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		var since *time.Time
		if !lastFull.IsZero() && now.Sub(lastFull) < fullInterval {
			since = &lastPass
		}
		runCtx, cancel := context.WithTimeout(ctx, ratePlanSweepDeadline)
		written, err := job.RunOnce(runCtx, now, since)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("rate plan sweep failed", "error", err)
		} else if written > 0 {
			slog.Info("rate plan sweep wrote tariff revisions", "count", written)
		}
		if since == nil {
			lastFull = now
		}
		lastPass = now
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
