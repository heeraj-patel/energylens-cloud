package catalogimport

import (
	"os"
	"path/filepath"
	"testing"
)

// This checked-in fixture was emitted by energylens-kb's Python
// write_signed_publication using a disposable test key. The private key is
// intentionally absent. It checks the actual Python/Go canonical bytes and
// Ed25519 transcript rather than signing with the Go verifier itself.
func TestPythonSignedPublicationFixture(t *testing.T) {
	artifact, err := os.ReadFile("testdata/python-reviewed-facts-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := os.ReadFile("testdata/python-reviewed-facts-v1.pub")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := ParseAndVerify(artifact, publicKey)
	if err != nil {
		t.Fatalf("Python-signed reviewed facts did not verify: %v", err)
	}
	if len(verified.Envelope.Payload.Products) != 2 {
		t.Fatalf("fixture product count = %d", len(verified.Envelope.Payload.Products))
	}
}

func TestRejectWritablePinnedPublicKey(t *testing.T) {
	artifact, err := os.ReadFile("testdata/python-reviewed-facts-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := os.ReadFile("testdata/python-reviewed-facts-v1.pub")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "artifact.json")
	keyPath := filepath.Join(dir, "publisher.pub")
	if err = os.WriteFile(artifactPath, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, publicKey, 0o666); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile is subject to umask; force the unsafe mode for this check.
	if err = os.Chmod(keyPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadAndVerify(artifactPath, keyPath); err == nil {
		t.Fatal("accepted a writable public key trust anchor")
	}
}
