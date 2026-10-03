package app

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"os"
)

// readMeteredExportKey reads raw random bytes from a private, persistent file.
func readMeteredExportKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("metered export cursor key file required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat metered export cursor key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 32 || info.Size() > 4096 {
		return nil, errors.New("metered export cursor key must be a private regular file containing 32-4096 random bytes")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metered export cursor key: %w", err)
	}
	if len(key) < 32 || len(key) > 4096 {
		return nil, errors.New("metered export cursor key must contain 32-4096 random bytes")
	}
	return key, nil
}

func (c Config) meteredExportKeys() ([]byte, []byte, error) {
	active, err := readMeteredExportKey(c.MeteredExportCursorKeyFile)
	if err != nil {
		return nil, nil, err
	}
	if c.MeteredExportPreviousKeyFile == "" {
		return active, nil, nil
	}
	previous, err := readMeteredExportKey(c.MeteredExportPreviousKeyFile)
	if err != nil {
		return nil, nil, err
	}
	if hmac.Equal(active, previous) {
		return nil, nil, errors.New("metered export active and previous cursor keys must differ")
	}
	return active, previous, nil
}
