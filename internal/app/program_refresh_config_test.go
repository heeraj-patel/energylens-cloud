package app

import (
	"strings"
	"testing"
	"time"
)

func TestProgramRefreshWorkerRequiresCompleteOEDIFiles(t *testing.T) {
	base := Config{Role: RoleWorker, Environment: "dev", APIListen: "127.0.0.1:0"}
	if err := base.Validate(); err != nil {
		t.Fatalf("default-off worker rejected: %v", err)
	}
	base.ProgramRefreshEnabled = true
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "both full OEDI CSVs") {
		t.Fatalf("incomplete worker accepted: %v", err)
	}
	base.DatabaseURL = "postgres://localhost/test"
	base.OEDIIOUPath = "/tmp/iou_zipcodes_2024.csv"
	base.OEDINonIOUPath = "/tmp/non_iou_zipcodes_2024.csv"
	base.OEDIIOUSHA256 = strings.Repeat("a", 64)
	base.OEDINonIOUSHA256 = strings.Repeat("b", 64)
	base.OEDIRetrievedAt = time.Now().UTC().Add(-time.Hour)
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "invalid OEDI program refresh files") {
		t.Fatalf("missing files accepted: %v", err)
	}
	base.Role = RoleAPI
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "worker or all") {
		t.Fatalf("API role accepted refresh worker: %v", err)
	}
}

func TestProgramSourceProbeRequiresRefreshWorker(t *testing.T) {
	c := Config{Role: RoleWorker, Environment: "dev", APIListen: "127.0.0.1:0", ProgramSourceProbeEnabled: true}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "program source probe requires program refresh") {
		t.Fatalf("source probe without refresh: %v", err)
	}
}
