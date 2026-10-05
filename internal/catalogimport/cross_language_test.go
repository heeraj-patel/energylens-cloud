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

func TestKBPythonSignedAnnualV2Fixtures(t *testing.T) {
	publicKey, err := os.ReadFile("testdata/kb-python-signed-annual-v2.pub.pem")
	if err != nil {
		t.Fatal(err)
	}
	verify := func(name string) Verified {
		t.Helper()
		artifact, readErr := os.ReadFile(filepath.Join("testdata", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		verified, verifyErr := ParseAndVerify(artifact, publicKey)
		if verifyErr != nil {
			t.Fatalf("KB Python-signed fixture %s did not verify: %v", name, verifyErr)
		}
		return verified
	}
	factCount := func(verified Verified) int {
		total := 0
		for _, product := range verified.Envelope.Payload.Products {
			total += len(product.Facts)
		}
		return total
	}

	prior := verify("kb-python-signed-annual-v2-prior.json")
	if prior.Envelope.Payload.SchemaVersion != 1 || factCount(prior) != 2 || prior.Envelope.Payload.Revision != nil {
		t.Fatalf("unexpected v1 prior fixture: schema=%d facts=%d revision=%+v", prior.Envelope.Payload.SchemaVersion, factCount(prior), prior.Envelope.Payload.Revision)
	}

	replacement := verify("kb-python-signed-annual-v2-replacement.json")
	if replacement.Envelope.Payload.SchemaVersion != 1 || factCount(replacement) != 1 || replacement.Envelope.Payload.Revision != nil {
		t.Fatalf("unexpected v1 replacement fixture: schema=%d facts=%d revision=%+v", replacement.Envelope.Payload.SchemaVersion, factCount(replacement), replacement.Envelope.Payload.Revision)
	}
	replacementID := replacement.Envelope.Payload.Products[0].Facts[0].ID

	retract := verify("kb-python-signed-annual-v2-retract.json")
	if payload := retract.Envelope.Payload; payload.SchemaVersion != 2 || payload.Revision == nil || payload.Revision.Action != "retract" || factCount(retract) != 0 || payload.Revision.ReplacementFactID != nil {
		t.Fatalf("unexpected v2 retract fixture: schema=%d facts=%d revision=%+v", payload.SchemaVersion, factCount(retract), payload.Revision)
	}

	supersede := verify("kb-python-signed-annual-v2-supersede.json")
	if payload := supersede.Envelope.Payload; payload.SchemaVersion != 2 || payload.Revision == nil || payload.Revision.Action != "supersede" || factCount(supersede) != 1 || payload.Revision.ReplacementFactID == nil || *payload.Revision.ReplacementFactID != replacementID || payload.Products[0].Facts[0].ID != replacementID {
		t.Fatalf("unexpected v2 supersede fixture: schema=%d facts=%d revision=%+v replacement_id=%s", payload.SchemaVersion, factCount(supersede), payload.Revision, replacementID)
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
