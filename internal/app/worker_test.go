package app

import (
	"context"
	"testing"
	"time"

	"energylens.local/cloud/internal/identityscan"
	"energylens.local/cloud/internal/meteredrollup"
)

type waitingRollup struct {
	started chan struct{}
	stopped chan struct{}
	t       *testing.T
}

type waitingIdentityScan struct {
	started chan struct{}
	stopped chan struct{}
	t       *testing.T
}

func (j waitingIdentityScan) Run(ctx context.Context, _ identityscan.Scanner, _ bool, limit int, _ time.Time) (int, error) {
	if limit != 10 {
		j.t.Errorf("unexpected identity batch limit %d", limit)
	}
	if _, ok := ctx.Deadline(); !ok {
		j.t.Error("identity scan pass missing deadline")
	}
	close(j.started)
	<-ctx.Done()
	close(j.stopped)
	return 0, ctx.Err()
}

func TestIdentityScanSchedulerStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	job := waitingIdentityScan{started: make(chan struct{}), stopped: make(chan struct{}), t: t}
	done := make(chan struct{})
	go func() { runIdentityScans(ctx, job, nil, time.Minute); close(done) }()
	select {
	case <-job.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("identity pass did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("identity pass did not stop")
	}
	select {
	case <-job.stopped:
	default:
		t.Fatal("scheduler stopped before pass")
	}
}

func TestNightlyRetryWindow(t *testing.T) {
	now := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	if !nightlyScanDue(now, "2026-10-01") || nightlyScanDue(now, "2026-10-02") || nightlyScanDue(now.Add(-time.Hour), "") {
		t.Fatal("nightly retry window incorrect")
	}
}

func TestKBScanRequiresWorkerConfiguration(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable", KBScanPython: "/usr/bin/python3", KBScanSources: "/private/sources"}
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted private KB worker")
	}
	base.Role = RoleWorker
	if err := base.Validate(); err != nil {
		t.Fatalf("worker config rejected: %v", err)
	}
	base.KBScanSources = ""
	if err := base.Validate(); err == nil {
		t.Fatal("partial KB config accepted")
	}
}

func (j waitingRollup) RunOnce(ctx context.Context, now time.Time, batches, buckets, purge int) (meteredrollup.Stats, error) {
	if batches != 10 || buckets != 100 || purge != 100 {
		j.t.Errorf("unexpected pass limits: %d/%d/%d", batches, buckets, purge)
	}
	if now.Location() != time.UTC {
		j.t.Error("rollup timestamp is not UTC")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > rollupDeadline {
		j.t.Error("rollup pass has no bounded deadline")
	}
	close(j.started)
	<-ctx.Done()
	close(j.stopped)
	return meteredrollup.Stats{}, ctx.Err()
}

func TestRollupStopsBeforePoolClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	job := waitingRollup{started: make(chan struct{}), stopped: make(chan struct{}), t: t}
	done := make(chan struct{})
	go func() {
		runMeteredRollups(ctx, job, time.Minute)
		close(done)
	}()
	select {
	case <-job.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("rollup did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rollup did not stop on cancellation")
	}
	select {
	case <-job.stopped:
	default:
		t.Fatal("scheduler returned before pass stopped")
	}
}

func TestRollupRequiresWorkerAndDatabase(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", MeteredRollupEnabled: true}
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted enabled rollup")
	}
	base.Role = RoleWorker
	if err := base.Validate(); err == nil {
		t.Fatal("worker accepted enabled rollup without database")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	if err := base.Validate(); err != nil {
		t.Fatalf("worker rejected configured rollup: %v", err)
	}
	base.Role = RoleAll
	if err := base.Validate(); err != nil {
		t.Fatalf("development all role rejected configured rollup: %v", err)
	}
}

func TestRollupEnvironmentFlag(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED", "true")
	if _, err := LoadConfig("api"); err == nil {
		t.Fatal("API role accepted enabled worker job")
	}
	if _, err := LoadConfig("worker"); err == nil {
		t.Fatal("enabled job accepted missing database")
	}
	t.Setenv("ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED", "maybe")
	if _, err := LoadConfig("worker"); err == nil {
		t.Fatal("invalid rollup flag accepted")
	}
	t.Setenv("ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED", "")
	if c, err := LoadConfig("worker"); err != nil || c.MeteredRollupEnabled {
		t.Fatalf("default should be disabled: enabled=%v err=%v", c.MeteredRollupEnabled, err)
	}
}

type waitingReportRetention struct {
	started chan struct{}
	t       *testing.T
}

func (j waitingReportRetention) RunOnce(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit != reportRetentionLimit || now.Location() != time.UTC {
		j.t.Errorf("unexpected retention pass arguments: limit=%d now=%v", limit, now)
	}
	if _, ok := ctx.Deadline(); !ok {
		j.t.Error("retention pass missing deadline")
	}
	close(j.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestReportRetentionWorkerConfigurationAndShutdown(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", ReportRetentionEnabled: true}
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted report retention")
	}
	base.Role = RoleWorker
	if err := base.Validate(); err == nil {
		t.Fatal("retention accepted missing database")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	if err := base.Validate(); err != nil {
		t.Fatalf("worker rejected retention: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := waitingReportRetention{started: make(chan struct{}), t: t}
	done := make(chan struct{})
	go func() { runReportRetention(ctx, job, time.Minute); close(done) }()
	select {
	case <-job.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("retention did not run at startup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention did not stop on cancellation")
	}
}

func TestReportRetentionEnvironmentFlag(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_REPORT_RETENTION_ENABLED", "maybe")
	if _, err := LoadConfig("worker"); err == nil {
		t.Fatal("invalid retention flag accepted")
	}
	t.Setenv("ENERGYLENS_CLOUD_REPORT_RETENTION_ENABLED", "")
	if c, err := LoadConfig("worker"); err != nil || c.ReportRetentionEnabled {
		t.Fatalf("default retention should be disabled: enabled=%v err=%v", c.ReportRetentionEnabled, err)
	}
}

func TestResendConfigurationIsWorkerOnlyAndDoesNotEnableDelivery(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable", ResendAPIKey: "test-key", ResendFrom: "reports@example.test"}
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted email provider credentials")
	}
	base.Role = RoleWorker
	if err := base.Validate(); err != nil {
		t.Fatalf("worker rejected paired provider configuration: %v", err)
	}
	base.ResendFrom = ""
	if err := base.Validate(); err == nil {
		t.Fatal("worker accepted partial provider configuration")
	}
	t.Setenv("ENERGYLENS_CLOUD_RESEND_API_KEY", "")
	t.Setenv("ENERGYLENS_CLOUD_RESEND_FROM", "")
	loaded, err := LoadConfig("worker")
	if err != nil || loaded.ResendAPIKey != "" || loaded.ResendFrom != "" {
		t.Fatalf("provider should default unconfigured: %+v %v", loaded, err)
	}
}
