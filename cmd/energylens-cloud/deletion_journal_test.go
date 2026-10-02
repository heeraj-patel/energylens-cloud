package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestDeletionJournalConfigFailsClosed(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_DELETION_JOURNAL_DIR", t.TempDir())
	t.Setenv("ENERGYLENS_CLOUD_DELETION_JOURNAL_KEY_FILE", "")
	if err := replayDeletionJournalBeforeServe(context.Background()); err == nil {
		t.Fatal("partial configuration accepted")
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte(hex.EncodeToString(make([]byte, 32))), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENERGYLENS_CLOUD_DELETION_JOURNAL_KEY_FILE", key)
	if _, _, err := deletionJournalConfig(); err == nil {
		t.Fatal("public key file accepted")
	}
	if err := os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, enabled, err := deletionJournalConfig(); err != nil || !enabled {
		t.Fatalf("private key: %v %v", enabled, err)
	}
	if err := replayDeletionJournalBeforeServe(context.Background()); err == nil {
		t.Fatal("missing privileged database URL accepted")
	}
}
