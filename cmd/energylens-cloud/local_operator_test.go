package main

import (
	"context"
	"strings"
	"testing"
)

// Local operator commands must keep the provisioning credential separate from
// the customer API credential, and must reject a non-local database before IO.
func TestLocalOperatorDatabaseBoundary(t *testing.T) {
	t.Setenv("RUNTIME_ENV", "local")
	t.Setenv("ENERGYLENS_CLOUD_ENV", "")
	local := "postgres://customer:secret@127.0.0.1:1/energylens?sslmode=disable"
	t.Setenv("ENERGYLENS_CLOUD_DATABASE_URL", local)
	t.Setenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL", local)
	if err := operator(context.Background(), []string{"import-reviewed-program"}); err == nil || !strings.Contains(err.Error(), "distinct database role") {
		t.Fatalf("same customer/operator role accepted: %v", err)
	}
	t.Setenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL", "postgres://operator:secret@db.example.com/energylens?sslmode=disable")
	if err := operator(context.Background(), []string{"import-reviewed-program"}); err == nil || !strings.Contains(err.Error(), "ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL") {
		t.Fatalf("public operator database accepted: %v", err)
	}
}
