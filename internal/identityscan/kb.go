package identityscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

type cappedBuffer struct {
	bytes.Buffer
	Max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.Max {
		return 0, errors.New("KB report exceeded limit")
	}
	return b.Buffer.Write(p)
}

// KBScanner invokes the installed KB scanner with argv, never a shell. EPA
// network lookup is explicitly enabled only after provider/source review.
type KBScanner struct {
	Python, Sources, CandidateIndex string
	AllowEPA                        bool
}

func (s KBScanner) Validate() error {
	for _, path := range []string{s.Python, s.Sources} {
		if !filepath.IsAbs(path) {
			return errors.New("KB paths must be absolute")
		}
	}
	if info, err := os.Stat(s.Python); err != nil || info.IsDir() {
		return errors.New("KB Python executable missing")
	}
	if info, err := os.Stat(s.Sources); err != nil || !info.IsDir() {
		return errors.New("KB reviewed source directory missing")
	}
	if s.CandidateIndex != "" {
		if !filepath.IsAbs(s.CandidateIndex) {
			return errors.New("candidate index path must be absolute")
		}
		if info, err := os.Stat(s.CandidateIndex); err != nil || info.IsDir() {
			return errors.New("candidate index missing")
		}
	}
	return nil
}

func (s KBScanner) Scan(ctx context.Context, r Request) (json.RawMessage, error) {
	args := []string{"-m", "energylens_kb", "scan-energy", "--category", r.Category, "--manufacturer", r.Manufacturer, "--model", r.Model, "--fuel", r.Fuel, "--sources", s.Sources}
	if r.Variant != nil {
		args = append(args, "--variant", *r.Variant)
	}
	if s.CandidateIndex != "" {
		args = append(args, "--identity-candidates", s.CandidateIndex)
	}
	if !s.AllowEPA {
		args = append(args, "--skip-epa")
	}
	cmd := exec.CommandContext(ctx, s.Python, args...)
	output := cappedBuffer{Max: 262144}
	cmd.Stdout = &output
	cmd.Stderr = &bytes.Buffer{} // provider errors may contain private input
	if err := cmd.Run(); err != nil {
		return nil, errors.New("KB scan failed")
	}
	if output.Len() > 262144 {
		return nil, errors.New("KB report exceeded limit")
	}
	return json.RawMessage(bytes.TrimSpace(output.Bytes())), nil
}
