package account

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testDirectoryJournal(t *testing.T) DirectoryJournal {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return DirectoryJournal{Dir: dir, Key: key}
}

func TestDirectoryJournalAppendReplayAndTamper(t *testing.T) {
	j := testDirectoryJournal(t)
	e := DeletionEntry{Sequence: 9, Kind: "account", ObjectID: uuid.NewString(), DeletedAt: time.Now().UTC().Round(0)}
	if err := j.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(e); err != nil {
		t.Fatal(err)
	}
	got, err := j.ReadAll()
	if err != nil || len(got) != 1 || got[0] != e {
		t.Fatalf("read: %v %v", got, err)
	}
	files, err := os.ReadDir(j.Dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(j.Dir, files[0].Name())
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ReadAll(); err == nil {
		t.Fatal("tampered record accepted")
	}
	if err := j.Append(e); err == nil {
		t.Fatal("damaged existing record accepted")
	}
}

func TestDirectoryJournalRejectsMissingAndUnsafeStorage(t *testing.T) {
	j := testDirectoryJournal(t)
	e := DeletionEntry{Sequence: 1, Kind: "home", ObjectID: uuid.NewString(), DeletedAt: time.Now().UTC()}
	j.Dir = filepath.Join(j.Dir, "missing")
	if err := j.Append(e); err == nil {
		t.Fatal("missing archive accepted")
	}
	j.Dir = t.TempDir()
	if err := os.Chmod(j.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(e); err == nil {
		t.Fatal("world-readable archive accepted")
	}
	j.Key = []byte("short")
	if _, err := j.ReadAll(); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestDirectoryJournalRejectsWrongKeyAndUnexpectedFiles(t *testing.T) {
	j := testDirectoryJournal(t)
	e := DeletionEntry{Sequence: 1, Kind: "home", ObjectID: uuid.NewString(), DeletedAt: time.Now().UTC()}
	if err := j.Append(e); err != nil {
		t.Fatal(err)
	}
	wrong := testDirectoryJournal(t)
	wrong.Dir = j.Dir
	if _, err := wrong.ReadAll(); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err := os.WriteFile(filepath.Join(j.Dir, "unrecognized"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ReadAll(); err == nil {
		t.Fatal("unexpected object accepted")
	}
}
