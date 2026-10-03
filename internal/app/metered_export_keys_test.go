package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMeteredExportCursorKeyConfiguration(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "staging", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://db.example/app?sslmode=verify-full", OIDCIssuer: "https://id.example/", OIDCAudience: "app", OIDCJWKSURL: "https://id.example/jwks"}
	if err := base.Validate(); err == nil {
		t.Fatal("owner API accepted missing cursor key")
	}
	base.Environment = "test"
	if _, _, err := base.meteredExportKeys(); err == nil {
		t.Fatal("owner route accepted missing cursor key")
	}
	dir := t.TempDir()
	active := filepath.Join(dir, "active.key")
	previous := filepath.Join(dir, "previous.key")
	keyA := bytes.Repeat([]byte{0x71}, 32)
	keyB := bytes.Repeat([]byte{0x42}, 32)
	if err := os.WriteFile(active, keyA, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, keyB, 0600); err != nil {
		t.Fatal(err)
	}
	base.MeteredExportCursorKeyFile, base.MeteredExportPreviousKeyFile = active, previous
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	gotA, gotB, err := base.meteredExportKeys()
	if err != nil || !bytes.Equal(gotA, keyA) || !bytes.Equal(gotB, keyB) {
		t.Fatalf("key rotation not loaded: %v", err)
	}
	if err := os.Chmod(active, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := base.meteredExportKeys(); err == nil {
		t.Fatal("world-readable cursor key accepted")
	}
}
