package catalogimport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fixture(t *testing.T) ([]byte, []byte, Payload) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	basis := map[string]json.RawMessage{"usage_basis": json.RawMessage(`"annual official"`), "matched_model": json.RawMessage(`"A123"`), "profile_contract_version": json.RawMessage(`"v1"`)}
	f := Fact{ID: uuid.NewString(), Kind: "annual_energy", Value: "123.4", Unit: "kWh", TestBasis: basis, SourceKind: "epa_exact", SourceURL: "https://example.test/official", SourceHash: strings.Repeat("a", 64), SourceHashBasis: "reviewed_row_manifest", SourceManifest: "sources/epa/a.json", ReviewedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), Revision: 1}
	f.IncludedEnergy.Status = "not_specified_by_source"
	p := Payload{Schema: "energylens.reviewed-catalog-facts", SchemaVersion: 1, Products: []Product{{ID: uuid.NewString(), Category: "refrigerator", Brand: "A", FullModel: "A123", Fuel: "electricity", MatchQuality: "confirmed_exact", Facts: []Fact{f}}}}
	envelope := signFixture(t, p, der, priv)
	return envelope, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), p
}
func signFixture(t *testing.T, p Payload, der []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	payload, err := canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	keyID := sha256.Sum256(der)
	envelope := Envelope{SchemaVersion: 1, Algorithm: "Ed25519", Payload: p, PayloadSHA256: hex.EncodeToString(hash[:]), KeyID: hex.EncodeToString(keyID[:]), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(messagePrefix), payload...)))}
	raw, err := canonical(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestVerifySignedReviewedPublication(t *testing.T) {
	artifact, key, _ := fixture(t)
	verified, err := ParseAndVerify(artifact, key)
	if err != nil {
		t.Fatal(err)
	}
	if verified.PayloadHash == "" || verified.EnvelopeHash == "" {
		t.Fatal("missing verified hashes")
	}
	if _, err = ParseAndVerify(bytes.Replace(artifact, []byte("123.4"), []byte("123.5"), 1), key); err == nil {
		t.Fatal("tampered value accepted")
	}
	if _, err = ParseAndVerify(append([]byte(" "), artifact...), key); err == nil {
		t.Fatal("noncanonical envelope accepted")
	}
	other, _, _ := fixture(t)
	if _, err = ParseAndVerify(other, key); err == nil {
		t.Fatal("untrusted key accepted")
	}
}
func TestRejectDuplicateAndUnsupportedPublication(t *testing.T) {
	artifact, key, p := fixture(t)
	var e Envelope
	if err := json.Unmarshal(artifact, &e); err != nil {
		t.Fatal(err)
	}
	e.SchemaVersion = 2
	raw, _ := canonical(e)
	if _, err := ParseAndVerify(raw, key); err == nil {
		t.Fatal("unsupported version accepted")
	}
	// Validation is also applied after a valid signature; a duplicate ID cannot
	// be smuggled into a signed publication.
	p.Products = append(p.Products, p.Products[0])
	if err := validatePayload(p); err == nil {
		t.Fatal("duplicate product accepted")
	}
	p.Products = p.Products[:1]
	p.Products[0].Facts = append(p.Products[0].Facts, p.Products[0].Facts[0])
	if err := validatePayload(p); err == nil {
		t.Fatal("duplicate fact accepted")
	}
}
