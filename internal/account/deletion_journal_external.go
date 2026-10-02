package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// JournalArchive is independent of the database backup. An implementation must
// durably create each record before returning and must reject corrupt records.
type JournalArchive interface {
	Append(DeletionEntry) error
	ReadAll() ([]DeletionEntry, error)
}

// DirectoryJournal stores immutable, authenticated records on a separately
// backed-up filesystem. The signing key must be stored independently of it.
type DirectoryJournal struct {
	Dir string
	Key []byte
}

type signedDeletion struct {
	Version int           `json:"version"`
	Entry   DeletionEntry `json:"entry"`
	MAC     string        `json:"mac"`
}

func validDeletionEntry(e DeletionEntry) error {
	if e.Sequence < 1 || (e.Kind != "home" && e.Kind != "account") || e.DeletedAt.IsZero() {
		return errors.New("invalid deletion journal entry")
	}
	if _, err := uuid.Parse(e.ObjectID); err != nil {
		return errors.New("invalid deletion journal object ID")
	}
	return nil
}

func (j DirectoryJournal) check() error {
	if len(j.Key) < 32 {
		return errors.New("deletion journal HMAC key must be at least 32 bytes")
	}
	if j.Dir == "" {
		return errors.New("deletion journal directory required")
	}
	st, err := os.Lstat(j.Dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("deletion journal directory must be a real directory")
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("deletion journal directory must have mode 0700")
	}
	return nil
}

func (j DirectoryJournal) encode(e DeletionEntry) (string, []byte, error) {
	if err := validDeletionEntry(e); err != nil {
		return "", nil, err
	}
	e.DeletedAt = e.DeletedAt.UTC().Round(0)
	payload, err := json.Marshal(struct {
		Version int           `json:"version"`
		Entry   DeletionEntry `json:"entry"`
	}{1, e})
	if err != nil {
		return "", nil, err
	}
	mac := hmac.New(sha256.New, j.Key)
	mac.Write(payload)
	envelope, err := json.Marshal(signedDeletion{1, e, hex.EncodeToString(mac.Sum(nil))})
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(envelope)
	return hex.EncodeToString(digest[:]) + ".json", append(envelope, '\n'), nil
}

func (j DirectoryJournal) Append(e DeletionEntry) error {
	if err := j.check(); err != nil {
		return err
	}
	name, data, err := j.encode(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.Dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	target := filepath.Join(j.Dir, name)
	if err = os.Link(tmp.Name(), target); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		existing, readErr := os.ReadFile(target)
		if readErr != nil {
			return readErr
		}
		if string(existing) != string(data) {
			return errors.New("deletion journal hash collision or damaged existing record")
		}
	}
	dir, err := os.Open(j.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (j DirectoryJournal) ReadAll() ([]DeletionEntry, error) {
	if err := j.check(); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(j.Dir)
	if err != nil {
		return nil, err
	}
	out := make([]DeletionEntry, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".pending-") {
			continue
		}
		if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".json") {
			return nil, fmt.Errorf("unexpected deletion journal object %q", f.Name())
		}
		data, err := os.ReadFile(filepath.Join(j.Dir, f.Name()))
		if err != nil {
			return nil, err
		}
		if len(data) > 1024 || len(data) == 0 {
			return nil, errors.New("invalid deletion journal record length")
		}
		var env signedDeletion
		if err = json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		if env.Version != 1 {
			return nil, errors.New("unsupported deletion journal version")
		}
		name, canonical, err := j.encode(env.Entry)
		if err != nil {
			return nil, err
		}
		if name != f.Name() || string(canonical) != string(data) {
			return nil, fmt.Errorf("invalid deletion journal signature or checksum: %s", f.Name())
		}
		out = append(out, env.Entry)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Sequence == out[k].Sequence {
			return out[i].ObjectID < out[k].ObjectID
		}
		return out[i].Sequence < out[k].Sequence
	})
	return out, nil
}

// ExportDeletionJournal scans from zero each time: PostgreSQL sequences are
// allocated before commit, so a saved cursor can skip a late commit.
func (s PGStore) ExportDeletionJournal(ctx context.Context, dst JournalArchive) (int, error) {
	after, count := int64(0), 0
	for {
		page, err := s.JournalSince(ctx, after, 1000)
		if err != nil {
			return count, err
		}
		if len(page) == 0 {
			return count, nil
		}
		for _, e := range page {
			if err := dst.Append(e); err != nil {
				return count, err
			}
			count++
		}
		after = page[len(page)-1].Sequence
	}
}

// ReplayDeletionJournal validates the complete archive before mutating the DB.
// It must run before restored database traffic is admitted.
func (s PGStore) ReplayDeletionJournal(ctx context.Context, src JournalArchive) (int, error) {
	entries, err := src.ReadAll()
	if err != nil {
		return 0, err
	}
	for i, e := range entries {
		if err := s.ReapplyDeletion(ctx, e); err != nil {
			return i, err
		}
	}
	return len(entries), nil
}
