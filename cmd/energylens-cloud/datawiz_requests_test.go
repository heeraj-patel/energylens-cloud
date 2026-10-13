package main

import (
	"context"
	"strings"
	"testing"
)

func TestDatawizOnePassExportRequiresAbsoluteOutputAndDatabase(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", "")
	for _, args := range [][]string{{}, {"-output", "relative/requests"}, {"-output", "/tmp/requests", "extra"}} {
		if err := exportDatawizRequests(context.Background(), args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if err := exportDatawizRequests(context.Background(), []string{"-output", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "ENERGYLENS_CLOUD_DATABASE_URL required") {
		t.Fatalf("database gate: %v", err)
	}
}
