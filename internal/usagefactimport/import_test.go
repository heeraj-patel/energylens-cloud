package usagefactimport

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
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Payload, []byte, ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	mode := "Normal Cycle"
	f := Fact{ID: "83ed216f-1992-57c7-9fcb-e1678e8c7879", Category: "washer", Manufacturer: "LG", Model: "WM3070HWA", Fuel: "electricity", Variant: "US-120V", MatchQuality: "documented_variant_family", DocumentedScope: "WM3070H*A", MatchedModel: "WM3070H*A", MeasurementKind: "cycle_energy", Value: "0.42", Unit: "kWh/cycle", TestBasis: "DOE Normal Cycle", CycleDefinition: &mode, IncludedEnergy: IncludedEnergy{Status: "documented", Components: []string{"appliance_electricity"}, EvidenceText: "includes appliance electricity and excludes water heating"}, SourceKind: "oem_pdf", SourceURL: "https://www.lg.com/us/support/wm3070-manual.pdf", SourceSHA256: strings.Repeat("a", 64), SourcePageIndex: &index, ReviewSHA256: strings.Repeat("b", 64), Reviewer: "fixture reviewer", ReviewedAt: "2026-09-28T00:00:00Z", SourceManifest: "washer.json"}
	p := Payload{Schema: "energylens.reviewed-usage-facts", SchemaVersion: 1, Facts: []Fact{f}}
	return p, der, priv, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
func signFixture(t *testing.T, p Payload, der []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	payload, err := canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	keyID := sha256.Sum256(der)
	env := Envelope{SchemaVersion: 1, Algorithm: "Ed25519", Payload: p, PayloadSHA256: hex.EncodeToString(digest[:]), KeyID: hex.EncodeToString(keyID[:]), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(domain), payload...)))}
	raw, err := canonical(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSignedUsagePublication(t *testing.T) {
	p, der, priv, key := fixture(t)
	raw := signFixture(t, p, der, priv)
	verified, err := ParseAndVerify(raw, key)
	if err != nil || verified.PayloadHash == "" || verified.EnvelopeHash == "" {
		t.Fatalf("valid artifact: %+v %v", verified, err)
	}
	if _, err = ParseAndVerify(bytes.Replace(raw, []byte("0.42"), []byte("0.43"), 1), key); err == nil {
		t.Fatal("tampered value accepted")
	}
	if _, err = ParseAndVerify(append([]byte(" "), raw...), key); err == nil {
		t.Fatal("noncanonical envelope accepted")
	}
	_, _, _, otherKey := fixture(t)
	if _, err = ParseAndVerify(raw, otherKey); err == nil {
		t.Fatal("wrong pinned key accepted")
	}
	if _, err = ParseAndVerify(raw, []byte("not a key")); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestPythonSignedUsageFixture(t *testing.T) {
	artifact, err := os.ReadFile("testdata/signed_usage_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile("testdata/signed_usage_fixture.pub.pem")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := ParseAndVerify(artifact, key)
	if err != nil {
		t.Fatalf("KB Python publication is incompatible: %v", err)
	}
	if len(verified.Envelope.Payload.Facts) != 1 || verified.Envelope.Payload.Facts[0].Unit != "kWh/cycle" {
		t.Fatal("unexpected KB fixture content")
	}
}
func TestNegativeUsageFacts(t *testing.T) {
	p, der, priv, key := fixture(t)
	cases := map[string]func(*Fact){
		"annual kind":              func(f *Fact) { f.MeasurementKind = "annual_energy" },
		"unit mismatch":            func(f *Fact) { f.Unit = "kWh" },
		"missing basis":            func(f *Fact) { f.TestBasis = "" },
		"no included evidence":     func(f *Fact) { f.IncludedEnergy.EvidenceText = "" },
		"wrong scope":              func(f *Fact) { f.DocumentedScope = "OTHER*" },
		"wildcard consumes two":    func(f *Fact) { f.Model = "WM3070HWXA" },
		"pattern regex syntax":     func(f *Fact) { f.DocumentedScope = "WM3070H.*A"; f.MatchedModel = f.DocumentedScope },
		"pattern too broad":        func(f *Fact) { f.DocumentedScope = "WM*"; f.MatchedModel = f.DocumentedScope },
		"literal family too broad": func(f *Fact) { f.DocumentedScope = "W"; f.MatchedModel = "W" },
		"energy omits appliance":   func(f *Fact) { f.IncludedEnergy.Components = []string{"water_heating_electricity"} },
		"wrong source hash":        func(f *Fact) { f.SourceSHA256 = "bad" },
		"wrong review hash":        func(f *Fact) { f.ReviewSHA256 = "bad" },
		"missing reviewer":         func(f *Fact) { f.Reviewer = "" },
		"wrong source kind":        func(f *Fact) { f.SourceKind = "unreviewed" },
		"wrong source URL":         func(f *Fact) { f.SourceURL = "http://example.test" },
		"zero value":               func(f *Fact) { f.Value = "0" },
		"EPA without metadata":     func(f *Fact) { f.SourceKind = "epa_row" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := p
			candidate.Facts = append([]Fact(nil), p.Facts...)
			mutate(&candidate.Facts[0])
			if _, err := ParseAndVerify(signFixture(t, candidate, der, priv), key); err == nil {
				t.Fatal("invalid signed fact accepted")
			}
		})
	}
	candidate := p
	candidate.Facts = append(candidate.Facts, p.Facts[0])
	candidate.Facts[1].ID = "22222222-2222-5222-8222-222222222222"
	if _, err := ParseAndVerify(signFixture(t, candidate, der, priv), key); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	candidate.Facts[1].Model = "WM-3070-HWA"
	if _, err := ParseAndVerify(signFixture(t, candidate, der, priv), key); err == nil {
		t.Fatal("normalized model identity collision accepted")
	}
	raw := signFixture(t, p, der, priv)
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	payload := envelope["payload"].(map[string]any)
	facts := payload["facts"].([]any)
	delete(facts[0].(map[string]any), "reviewer")
	missing, err := canonical(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseAndVerify(missing, key); err == nil {
		t.Fatal("omitted reviewer accepted")
	}
}

func TestSignedGasUsageRequiresExactDryerThermBasis(t *testing.T) {
	p, der, priv, key := fixture(t)
	f := &p.Facts[0]
	f.Category = "dryer"
	f.Model = "DLGX5171V"
	f.DocumentedScope = f.Model
	f.MatchedModel = f.Model
	f.Fuel = "natural_gas"
	f.MatchQuality = "confirmed_exact"
	f.Unit = "therm/cycle"
	f.IncludedEnergy.Components = []string{"appliance_gas"}
	if _, err := ParseAndVerify(signFixture(t, p, der, priv), key); err != nil {
		t.Fatalf("exact gas fact rejected: %v", err)
	}
	for name, change := range map[string]func(*Fact){
		"electric unit":        func(f *Fact) { f.Unit = "kWh/cycle" },
		"family scope":         func(f *Fact) { f.MatchQuality = "documented_variant_family" },
		"other category":       func(f *Fact) { f.Category = "washer" },
		"electrical component": func(f *Fact) { f.IncludedEnergy.Components = []string{"appliance_electricity"} },
		"mixed components":     func(f *Fact) { f.IncludedEnergy.Components = []string{"appliance_gas", "appliance_electricity"} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := p
			candidate.Facts = append([]Fact(nil), p.Facts...)
			change(&candidate.Facts[0])
			if _, err := ParseAndVerify(signFixture(t, candidate, der, priv), key); err == nil {
				t.Fatal("incompatible gas fact accepted")
			}
		})
	}
}
