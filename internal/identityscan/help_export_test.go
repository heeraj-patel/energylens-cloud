package identityscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelpExportAtomicPrivateAndMinimal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unresolved_device_help.jsonl")
	date := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	record := HelpRecord{ApplianceID: "appliance-1", Manufacturer: "Acme", Model: "A1", Category: "dryer", Fuel: "electricity", NormalizedIdentity: normalizeIdentity("Dryer", " Acme ", "A1", "electricity"), LookupStatus: "missing_source", LookupDate: date}
	if err := writeHelpAtomic(path, []HelpRecord{record}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private output: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "home_id") || strings.Contains(string(data), "account") || strings.Contains(string(data), "credential") {
		t.Fatalf("private fields leaked: %s", data)
	}
	var got HelpRecord
	if err := json.Unmarshal(data, &got); err != nil || got != record || got.NormalizedIdentity != "dryer|acme|a1|electricity" {
		t.Fatalf("record: %+v %v", got, err)
	}
	if err := writeHelpAtomic(path, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || len(data) != 0 {
		t.Fatalf("stale queue retained: %q %v", data, err)
	}
}

func TestHelpExportRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unresolved_device_help.jsonl")
	if err := os.Symlink("target", path); err != nil {
		t.Fatal(err)
	}
	if err := writeHelpAtomic(path, nil); err == nil {
		t.Fatal("symlink accepted")
	}
}
