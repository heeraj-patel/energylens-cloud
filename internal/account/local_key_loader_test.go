package account

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLocalKeyLoaderAndRotation(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makePair := func(name string) (string, string) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		publicDER, err := x509.MarshalPKIXPublicKey(public)
		if err != nil {
			t.Fatal(err)
		}
		return write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600), write(name+".pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0644)
	}
	oldPrivate, oldPublic := makePair("old")
	newPrivate, newPublic := makePair("new")
	files := LocalKeyFiles{ActiveKeyID: "old", PrivatePath: oldPrivate, PublicPaths: map[string]string{"old": oldPublic}, Audience: "energylens-cloud"}
	old, err := LoadLocalAccessCodec(files)
	if err != nil {
		t.Fatal(err)
	}
	token, err := old.signVerified(uuid.NewString(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	files.ActiveKeyID = "new"
	files.PrivatePath = newPrivate
	files.PublicPaths["new"] = newPublic
	rotated, err := LoadLocalAccessCodec(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = rotated.verifySignature(token); err != nil {
		t.Fatalf("overlap rejected old token: %v", err)
	}
	delete(files.PublicPaths, "old")
	retired, err := LoadLocalAccessCodec(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = retired.verifySignature(token); err == nil {
		t.Fatal("retired key accepted")
	}
	files.PrivatePath = oldPrivate
	if _, err = LoadLocalAccessCodec(files); err == nil {
		t.Fatal("mismatched active key accepted")
	}
	files.PrivatePath = newPrivate
	if err = os.Chmod(newPrivate, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadLocalAccessCodec(files); err == nil {
		t.Fatal("world-readable private key accepted")
	}
	if err = os.Chmod(newPrivate, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "private-link")
	if err = os.Symlink(newPrivate, link); err != nil {
		t.Fatal(err)
	}
	files.PrivatePath = link
	if _, err = LoadLocalAccessCodec(files); err == nil {
		t.Fatal("private key symlink accepted")
	}
	files.PrivatePath = "relative.key"
	if _, err = LoadLocalAccessCodec(files); err == nil {
		t.Fatal("relative key path accepted")
	}
}

func TestLocalRateKeyLoaderRequiresPrivateBinaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadLocalRateKey(path); err != nil || len(got) != 32 {
		t.Fatalf("private rate key rejected: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLocalRateKey(path); err == nil {
		t.Fatal("world-readable rate key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, key[:31], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLocalRateKey(path); err == nil {
		t.Fatal("short rate key accepted")
	}
}
