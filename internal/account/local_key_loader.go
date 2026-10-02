package account

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// LocalKeyFiles is an explicit, immutable deployment snapshot. Callers must
// atomically replace the whole codec when rotating; no token controls a path.
type LocalKeyFiles struct {
	ActiveKeyID string
	PrivatePath string
	PublicPaths map[string]string
	Audience    string
}

// LoadLocalAccessCodec reads only absolute, regular, same-owner files without
// following final-component symlinks. It never logs key contents or paths.
func LoadLocalAccessCodec(files LocalKeyFiles) (LocalAccessCodec, error) {
	if !validLocalKeyID(files.ActiveKeyID) || files.Audience == "" || len(files.PublicPaths) == 0 || len(files.PublicPaths) > 8 {
		return LocalAccessCodec{}, errors.New("invalid local signing key configuration")
	}
	privatePEM, err := readLocalKeyFile(files.PrivatePath, true)
	if err != nil {
		return LocalAccessCodec{}, err
	}
	block, rest := pem.Decode(privatePEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return LocalAccessCodec{}, errors.New("invalid local private key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	private, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok || len(private) != ed25519.PrivateKeySize {
		return LocalAccessCodec{}, errors.New("local private key must be Ed25519 PKCS8")
	}
	keys := make(map[string]ed25519.PublicKey, len(files.PublicPaths))
	for id, path := range files.PublicPaths {
		if !validLocalKeyID(id) {
			return LocalAccessCodec{}, errors.New("invalid local public key ID")
		}
		data, err := readLocalKeyFile(path, false)
		if err != nil {
			return LocalAccessCodec{}, err
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			return LocalAccessCodec{}, errors.New("invalid local public key PEM")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		key, ok := parsed.(ed25519.PublicKey)
		if err != nil || !ok || len(key) != ed25519.PublicKeySize {
			return LocalAccessCodec{}, errors.New("local public key must be Ed25519 PKIX")
		}
		keys[id] = key
	}
	codec := LocalAccessCodec{Private: private, ActiveKeyID: files.ActiveKeyID, Keys: keys, Audience: files.Audience}
	if !codec.valid() {
		return LocalAccessCodec{}, errors.New("local active signing key does not match trusted public key")
	}
	return codec, nil
}

func readLocalKeyFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("local key path must be absolute")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open local key: %w", err)
	}
	file := os.NewFile(uintptr(fd), "local-key")
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > 8192 {
		return nil, errors.New("local key file must be a bounded regular file")
	}
	if stat.Mode().Perm()&0022 != 0 || private && stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local key file permissions are unsafe")
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("local key file owner mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) == 0 || len(data) > 8192 {
		return nil, errors.New("local key file is unreadable or oversized")
	}
	return data, nil
}
