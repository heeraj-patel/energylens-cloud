package app

import (
	"context"
	"log/slog"
	"time"

	"energylens.local/cloud/internal/identityscan"
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

type identityScanJob interface {
	Run(context.Context, identityscan.Scanner, bool, int, time.Time) (int, error)
	ExportHelp(context.Context, string) (int, error)
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

func nightlyScanDue(now time.Time, last string) bool {
	return now.UTC().Hour() == 3 && now.UTC().Format("2006-01-02") != last
}

// runIdentityScans responds to new identity outbox rows within one short poll
// interval. Only the 03:00 UTC pass includes due missing-source retries.
func runIdentityScans(ctx context.Context, job identityScanJob, scanner identityscan.Scanner, interval time.Duration, helpOutputPath string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastNightly := ""
	firstPass := true
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
		if err == nil && helpOutputPath != "" && (firstPass || nightly || count > 0) {
			_, err = job.ExportHelp(runCtx, helpOutputPath)
		}
		cancel()
		firstPass = false
		if err != nil && ctx.Err() == nil {
			slog.Error("identity scan or help export pass failed", "error", err)
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
