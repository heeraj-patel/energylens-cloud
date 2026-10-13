package ratedinputimport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"testing"
)

func signedFixture(t *testing.T, payload Payload, key ed25519.PrivateKey) ([]byte, []byte) {
	t.Helper()
	body, err := canonical(payload)
	if err != nil {
		t.Fatal(err)
	}
	bodyHash := sha256.Sum256(body)
	publicDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	keyHash := sha256.Sum256(publicDER)
	domain := []byte("energylens.reviewed-rated-input-specs.v1\n")
	if payload.SchemaVersion == 2 {
		domain = []byte("energylens.reviewed-rated-input-specs.v2\n")
	}
	env := map[string]any{"schema_version": payload.SchemaVersion, "algorithm": "Ed25519", "payload": payload, "payload_sha256": hex.EncodeToString(bodyHash[:]), "key_id": hex.EncodeToString(keyHash[:]), "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append(domain, body...)))}
	artifact, err := canonical(env)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	return artifact, publicPEM
}

func TestParseAndVerifyInitialAndRevisionPublications(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fact := Fact{ID: "8c2391b7-3321-4c4b-827b-a5e0429e64c1", Category: "vacuum", Manufacturer: "Shark", Model: "AZ1002", SourceKind: "oem_rated_input", MatchedModel: "AZ1000 Series", RatedInputWatts: "1350", Unit: "W", SourceURL: "https://example.com/spec.pdf", ManufacturerSourceURL: "https://example.com/spec.pdf", SourceSHA256: hex.EncodeToString(make([]byte, 32)), ReviewSHA256: hex.EncodeToString(make([]byte, 32)), Reviewer: "reviewer", ReviewedAt: 1790726400, UsageBasis: "rated nameplate input", SourceManifest: "shark.json"}
	for _, payload := range []Payload{
		{Schema: schema, SchemaVersion: 1, Facts: []Fact{fact}},
		{Schema: schema, SchemaVersion: 2, Facts: []Fact{}, Revision: &Revision{ID: "80a1c769-7730-46e0-9c42-ad75455ef43a", Action: "retract", PriorFactID: fact.ID, Reason: "source correction", Reviewer: "reviewer", ReviewedAt: "2026-10-10T12:00:00Z", ReviewSHA256: hex.EncodeToString(make([]byte, 32))}},
	} {
		artifact, public := signedFixture(t, payload, key)
		if _, err := ParseAndVerify(artifact, public); err != nil {
			t.Fatalf("valid v%d rejected: %v", payload.SchemaVersion, err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(artifact, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["signature"] = "invalid"
		bad, _ := json.Marshal(envelope)
		if _, err := ParseAndVerify(bad, public); err == nil {
			t.Fatalf("tampered v%d accepted", payload.SchemaVersion)
		}
	}
}
