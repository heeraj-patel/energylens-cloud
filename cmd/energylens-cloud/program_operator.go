package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"energylens.local/cloud/internal/programs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type reviewedProgramArtifact struct {
	programs.ReviewedImport
	SourceSHA256 string `json:"source_sha256"`
}

// The operator supplies an independently captured official artifact. This
// command performs no network fetch and cannot register a source implicitly.
func importReviewedProgram(ctx context.Context, pool *pgxpool.Pool, fs *flag.FlagSet, args []string) (map[string]string, error) {
	artifactPath := fs.String("artifact", "", "reviewed program JSON")
	snapshotPath := fs.String("source-snapshot", "", "exact official source bytes")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 || *artifactPath == "" || *snapshotPath == "" {
		return nil, errors.New("artifact and source-snapshot required")
	}
	artifact, err := readBoundedProgramFile(*artifactPath, 128<<10)
	if err != nil {
		return nil, err
	}
	snapshot, err := readBoundedProgramFile(*snapshotPath, 16<<20)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(artifact))
	decoder.DisallowUnknownFields()
	var in reviewedProgramArtifact
	if err = decoder.Decode(&in); err != nil {
		return nil, err
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("one reviewed program JSON object required")
	}
	if len(in.SourceBytes) != 0 || in.ExistingSnapshotID != uuid.Nil {
		return nil, errors.New("source bytes and snapshot ID must come from --source-snapshot")
	}
	digest := sha256.Sum256(snapshot)
	hash := hex.EncodeToString(digest[:])
	if in.SourceSHA256 != hash {
		return nil, fmt.Errorf("source snapshot SHA-256 mismatch: captured bytes are %s", hash)
	}
	in.SourceBytes = snapshot
	id, err := programs.ImportReviewed(ctx, pool, in.ReviewedImport)
	if err != nil {
		return nil, err
	}
	return map[string]string{"revision_id": id.String(), "source_sha256": hash}, nil
}

func readBoundedProgramFile(path string, maximum int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > maximum {
		return nil, errors.New("program artifact or source snapshot is empty or too large")
	}
	return data, nil
}
