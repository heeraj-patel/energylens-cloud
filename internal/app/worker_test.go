package app

import (
	"context"
	"testing"
	"time"

	"energylens.local/cloud/internal/meteredrollup"
)

type waitingRollup struct {
	started chan struct{}
	stopped chan struct{}
	t       *testing.T
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

type waitingKasaRuntimeRetention struct {
	started chan struct{}
	t       *testing.T
}

func (j waitingKasaRuntimeRetention) RunOnce(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit != kasaRuntimeRetentionLimit || now.Location() != time.UTC {
		j.t.Errorf("unexpected Kasa retention pass arguments: limit=%d now=%v", limit, now)
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > kasaRuntimeRetentionDeadline {
		j.t.Error("Kasa retention pass missing bounded deadline")
	}
	close(j.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestKasaRuntimeRetentionWorkerConfigurationAndShutdown(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", KasaRuntimeRetentionEnabled: true}
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted Kasa runtime retention")
	}
	base.Role = RoleWorker
	if err := base.Validate(); err == nil {
		t.Fatal("Kasa runtime retention accepted missing database")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	if err := base.Validate(); err != nil {
		t.Fatalf("worker rejected Kasa runtime retention: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := waitingKasaRuntimeRetention{started: make(chan struct{}), t: t}
	done := make(chan struct{})
	go func() { runKasaRuntimeRetention(ctx, job, time.Minute); close(done) }()
	select {
	case <-job.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("Kasa runtime retention did not run at startup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Kasa runtime retention did not stop on cancellation")
	}
}

func TestKasaRuntimeRetentionEnvironmentFlag(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_KASA_RUNTIME_RETENTION_ENABLED", "maybe")
	if _, err := LoadConfig("worker"); err == nil {
		t.Fatal("invalid Kasa runtime retention flag accepted")
	}
	t.Setenv("ENERGYLENS_CLOUD_KASA_RUNTIME_RETENTION_ENABLED", "")
	if c, err := LoadConfig("worker"); err != nil || c.KasaRuntimeRetentionEnabled {
		t.Fatalf("default Kasa runtime retention should be disabled: enabled=%v err=%v", c.KasaRuntimeRetentionEnabled, err)
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

type waitingReportDelivery struct {
	started chan struct{}
	stopped chan struct{}
	t       *testing.T
}

func (j waitingReportDelivery) DeliverOne(ctx context.Context, now time.Time) (bool, error) {
	if now.Location() != time.UTC {
		j.t.Errorf("delivery timestamp is not UTC: %v", now)
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > reportDeliveryDeadline {
		j.t.Error("delivery pass missing bounded deadline")
	}
	close(j.started)
	<-ctx.Done()
	close(j.stopped)
	return false, ctx.Err()
}

func TestReportDeliveryRequiresExplicitWorkerConfiguration(t *testing.T) {
	base := Config{Role: RoleWorker, Environment: "dev", APIListen: "127.0.0.1:0"}
	if err := base.Validate(); err != nil {
		t.Fatalf("disabled delivery rejected: %v", err)
	}
	base.ReportDeliveryEnabled = true
	if err := base.Validate(); err == nil {
		t.Fatal("delivery accepted missing database and provider")
	}
	base.DatabaseURL = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	base.ResendAPIKey = "test-key"
	base.ResendFrom = "reports@example.test"
	if err := base.Validate(); err != nil {
		t.Fatalf("configured worker delivery rejected: %v", err)
	}
	base.Role = RoleAPI
	if err := base.Validate(); err == nil {
		t.Fatal("API role accepted enabled delivery")
	}
	base.Role = RoleWorker
	base.ResendFrom = "bad sender"
	if err := base.Validate(); err == nil {
		t.Fatal("delivery accepted invalid provider sender")
	}
}

func TestReportDeliveryEnvironmentFlagAndShutdown(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_REPORT_DELIVERY_ENABLED", "maybe")
	if _, err := LoadConfig("worker"); err == nil {
		t.Fatal("invalid delivery flag accepted")
	}
	t.Setenv("ENERGYLENS_CLOUD_REPORT_DELIVERY_ENABLED", "")
	if c, err := LoadConfig("worker"); err != nil || c.ReportDeliveryEnabled {
		t.Fatalf("delivery should default disabled: enabled=%v err=%v", c.ReportDeliveryEnabled, err)
	}
	t.Setenv("ENERGYLENS_CLOUD_REPORT_DELIVERY_ENABLED", "true")
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	t.Setenv("ENERGYLENS_CLOUD_RESEND_API_KEY", "test-key")
	t.Setenv("ENERGYLENS_CLOUD_RESEND_FROM", "reports@example.test")
	if c, err := LoadConfig("worker"); err != nil || !c.ReportDeliveryEnabled {
		t.Fatalf("configured worker delivery flag rejected: enabled=%v err=%v", c.ReportDeliveryEnabled, err)
	}
	if _, err := LoadConfig("api"); err == nil {
		t.Fatal("API role accepted delivery flag")
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := waitingReportDelivery{started: make(chan struct{}), stopped: make(chan struct{}), t: t}
	done := make(chan struct{})
	go func() { runReportDeliveries(ctx, job, time.Minute); close(done) }()
	select {
	case <-job.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("delivery did not run at startup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivery did not stop on cancellation")
	}
	select {
	case <-job.stopped:
	default:
		t.Fatal("delivery scheduler returned before pass stopped")
	}
}
