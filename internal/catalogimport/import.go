// Package catalogimport verifies a reviewed KB publication and persists it
// through the operator-only database credential. It exposes no HTTP route.
package catalogimport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const messagePrefix = "energylens.reviewed-catalog-facts.v1\n"
const maxArtifactBytes = 16 << 20

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var decimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type Envelope struct {
	SchemaVersion int     `json:"schema_version"`
	Algorithm     string  `json:"algorithm"`
	Payload       Payload `json:"payload"`
	PayloadSHA256 string  `json:"payload_sha256"`
	KeyID         string  `json:"key_id"`
	Signature     string  `json:"signature"`
}
type Payload struct {
	Schema        string    `json:"schema"`
	SchemaVersion int       `json:"schema_version"`
	Products      []Product `json:"products"`
}
type Product struct {
	ID           string  `json:"id"`
	Category     string  `json:"category"`
	Brand        string  `json:"brand"`
	FullModel    string  `json:"full_model"`
	Variant      *string `json:"variant"`
	Region       *string `json:"region"`
	Fuel         string  `json:"fuel"`
	MatchQuality string  `json:"match_quality"`
	Facts        []Fact  `json:"facts"`
}
type Fact struct {
	ID             string                     `json:"id"`
	Kind           string                     `json:"kind"`
	Value          string                     `json:"value"`
	Unit           string                     `json:"unit"`
	TestBasis      map[string]json.RawMessage `json:"test_basis"`
	IncludedEnergy struct {
		Status string `json:"status"`
	} `json:"included_energy"`
	SourceKind      string `json:"source_kind"`
	SourceURL       string `json:"source_url"`
	SourceHash      string `json:"source_hash"`
	SourceHashBasis string `json:"source_hash_basis"`
	SourceManifest  string `json:"source_manifest"`
	ReviewedAt      string `json:"reviewed_at"`
	Revision        int    `json:"revision"`
}
type Verified struct {
	Envelope     Envelope
	EnvelopeHash string
	PayloadHash  string
}

func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&generic); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(generic); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func ParseAndVerify(artifact []byte, publicPEM []byte) (Verified, error) {
	if len(artifact) == 0 || len(artifact) > maxArtifactBytes {
		return Verified{}, errors.New("artifact size is invalid")
	}
	block, rest := pem.Decode(publicPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return Verified{}, errors.New("pinned key must be one PEM public key")
	}
	keyAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Verified{}, err
	}
	key, ok := keyAny.(ed25519.PublicKey)
	if !ok {
		return Verified{}, errors.New("pinned key is not Ed25519")
	}
	keyID := sha256.Sum256(block.Bytes)
	var generic any
	d := json.NewDecoder(bytes.NewReader(artifact))
	d.UseNumber()
	if err = d.Decode(&generic); err != nil {
		return Verified{}, err
	}
	var extra any
	if err = d.Decode(&extra); err == nil {
		return Verified{}, errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return Verified{}, err
	}
	canonicalEnvelope, err := canonical(generic)
	if err != nil {
		return Verified{}, err
	}
	if !bytes.Equal(artifact, canonicalEnvelope) {
		return Verified{}, errors.New("envelope is not canonical JSON")
	}
	var env Envelope
	if err = decodeEnvelope(artifact, &env); err != nil {
		return Verified{}, err
	}
	if env.SchemaVersion != 1 || env.Algorithm != "Ed25519" || env.Payload.Schema != "energylens.reviewed-catalog-facts" || env.Payload.SchemaVersion != 1 {
		return Verified{}, errors.New("unsupported publication format")
	}
	if env.KeyID != hex.EncodeToString(keyID[:]) {
		return Verified{}, errors.New("publication signed by unauthorized key")
	}
	payload, err := canonical(env.Payload)
	if err != nil {
		return Verified{}, err
	}
	digest := sha256.Sum256(payload)
	if env.PayloadSHA256 != hex.EncodeToString(digest[:]) {
		return Verified{}, errors.New("payload digest mismatch")
	}
	if len(env.Signature) != 86 || strings.Contains(env.Signature, "=") {
		return Verified{}, errors.New("invalid signature encoding")
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != env.Signature {
		return Verified{}, errors.New("invalid signature encoding")
	}
	if !ed25519.Verify(key, append([]byte(messagePrefix), payload...), sig) {
		return Verified{}, errors.New("signature verification failed")
	}
	if err = validatePayload(env.Payload); err != nil {
		return Verified{}, err
	}
	envelopeHash := sha256.Sum256(artifact)
	return Verified{Envelope: env, EnvelopeHash: hex.EncodeToString(envelopeHash[:]), PayloadHash: env.PayloadSHA256}, nil
}

func decodeEnvelope(raw []byte, out *Envelope) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err == nil {
		return errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
func validUUID(s string) bool { u, e := uuid.Parse(s); return e == nil && u.String() == s }
func nonblank(s string) bool  { return s != "" && strings.TrimSpace(s) == s }
func stringField(m map[string]json.RawMessage, k string) bool {
	var v string
	return json.Unmarshal(m[k], &v) == nil && nonblank(v)
}
func validatePayload(p Payload) error {
	if len(p.Products) == 0 {
		return errors.New("publication has no products")
	}
	products := map[string]bool{}
	facts := map[string]bool{}
	for _, product := range p.Products {
		if !validUUID(product.ID) || products[product.ID] || !nonblank(product.Category) || !nonblank(product.Brand) || !nonblank(product.FullModel) || product.Fuel != "electricity" {
			return errors.New("invalid or duplicate product")
		}
		products[product.ID] = true
		if product.MatchQuality != "confirmed_exact" && product.MatchQuality != "documented_variant_family" {
			return errors.New("invalid match quality")
		}
		if product.Variant != nil && !nonblank(*product.Variant) || product.Region != nil && !nonblank(*product.Region) || len(product.Facts) == 0 {
			return errors.New("invalid product details")
		}
		for _, fact := range product.Facts {
			if !validUUID(fact.ID) || facts[fact.ID] || fact.Kind != "annual_energy" || fact.Unit != "kWh" || fact.Revision != 1 {
				return errors.New("invalid or duplicate fact")
			}
			facts[fact.ID] = true
			if !decimal.MatchString(fact.Value) {
				return errors.New("invalid fact value")
			}
			value, ok := new(big.Rat).SetString(fact.Value)
			if !ok || value.Sign() <= 0 {
				return errors.New("fact value must be positive")
			}
			if !stringField(fact.TestBasis, "usage_basis") || !stringField(fact.TestBasis, "matched_model") || !stringField(fact.TestBasis, "profile_contract_version") || fact.IncludedEnergy.Status != "not_specified_by_source" {
				return errors.New("invalid test basis")
			}
			if fact.SourceKind != "epa_exact" && fact.SourceKind != "oem_exact" && fact.SourceKind != "oem_family" && fact.SourceKind != "other_official" {
				return errors.New("invalid source kind")
			}
			if (fact.SourceKind == "epa_exact" || fact.SourceKind == "oem_exact") != (product.MatchQuality == "confirmed_exact") {
				return errors.New("source kind and match quality disagree")
			}
			u, e := url.Parse(fact.SourceURL)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return errors.New("source URL must be HTTPS")
			}
			if !hexHash.MatchString(fact.SourceHash) || !nonblank(fact.SourceManifest) || !strings.HasPrefix(fact.SourceManifest, "sources/") || strings.Contains(fact.SourceManifest, "..") {
				return errors.New("invalid source provenance")
			}
			if fact.SourceHashBasis != "verified_source_document" && fact.SourceHashBasis != "reviewed_row_manifest" {
				return errors.New("invalid source hash basis")
			}
			reviewed, e := time.Parse(time.RFC3339, fact.ReviewedAt)
			if e != nil || reviewed.IsZero() || reviewed.After(time.Now().Add(5*time.Minute)) {
				return errors.New("invalid review time")
			}
		}
	}
	return nil
}

func ReadAndVerify(path, keyPath string) (Verified, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Verified{}, err
	}
	if !info.Mode().IsRegular() {
		return Verified{}, errors.New("artifact must be a regular file")
	}
	keyInfo, err := os.Lstat(keyPath)
	if err != nil {
		return Verified{}, err
	}
	if !keyInfo.Mode().IsRegular() {
		return Verified{}, errors.New("pinned key must be a regular file")
	}
	if keyInfo.Mode().Perm()&0o022 != 0 {
		return Verified{}, errors.New("pinned key must not be group or world writable")
	}
	artifact, err := os.ReadFile(path)
	if err != nil {
		return Verified{}, err
	}
	public, err := os.ReadFile(keyPath)
	if err != nil {
		return Verified{}, err
	}
	return ParseAndVerify(artifact, public)
}

type Result struct {
	PayloadSHA256 string `json:"payload_sha256"`
	Products      int    `json:"products"`
	Facts         int    `json:"facts"`
	Replayed      bool   `json:"replayed"`
}

func Import(ctx context.Context, pool *pgxpool.Pool, verified Verified, actor string) (Result, error) {
	if !nonblank(actor) {
		return Result{}, errors.New("imported-by is required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize all operator imports. This also protects duplicate IDs across publications.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(209321, 21)`); err != nil {
		return Result{}, err
	}
	var existingKey, existingEnvelope string
	err = tx.QueryRow(ctx, `SELECT key_id,envelope_sha256 FROM catalog_publications WHERE payload_sha256=$1`, verified.PayloadHash).Scan(&existingKey, &existingEnvelope)
	if err == nil {
		if existingKey != verified.Envelope.KeyID || existingEnvelope != verified.EnvelopeHash {
			return Result{}, errors.New("publication replay differs from prior envelope")
		}
		return Result{PayloadSHA256: verified.PayloadHash, Products: len(verified.Envelope.Payload.Products), Facts: countFacts(verified.Envelope.Payload), Replayed: true}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	result := Result{PayloadSHA256: verified.PayloadHash, Products: len(verified.Envelope.Payload.Products), Facts: countFacts(verified.Envelope.Payload)}
	_, err = tx.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$2,$3,$4,$5,$6)`, verified.PayloadHash, verified.Envelope.KeyID, verified.EnvelopeHash, actor, result.Products, result.Facts)
	if err != nil {
		return Result{}, err
	}
	for _, p := range verified.Envelope.Payload.Products {
		productJSON, _ := json.Marshal(struct {
			ID           string  `json:"id"`
			Category     string  `json:"category"`
			Brand        string  `json:"brand"`
			FullModel    string  `json:"full_model"`
			Variant      *string `json:"variant"`
			Region       *string `json:"region"`
			Fuel         string  `json:"fuel"`
			MatchQuality string  `json:"match_quality"`
		}{p.ID, p.Category, p.Brand, p.FullModel, p.Variant, p.Region, p.Fuel, p.MatchQuality})
		var old []byte
		err = tx.QueryRow(ctx, `SELECT product_json FROM catalog_products WHERE id=$1`, p.ID).Scan(&old)
		if err == nil {
			if !jsonEqual(old, productJSON) {
				return Result{}, fmt.Errorf("product %s conflicts with immutable identity", p.ID)
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,variant,region,fuel,match_quality,product_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.ID, p.Category, p.Brand, p.FullModel, p.Variant, p.Region, p.Fuel, p.MatchQuality, productJSON)
			if err != nil {
				return Result{}, err
			}
		} else {
			return Result{}, err
		}
		for _, f := range p.Facts {
			factJSON, _ := json.Marshal(f)
			var oldFact []byte
			var oldProduct string
			err = tx.QueryRow(ctx, `SELECT product_id,fact_json FROM catalog_fact_revisions WHERE id=$1`, f.ID).Scan(&oldProduct, &oldFact)
			if err == nil {
				if oldProduct != p.ID || !jsonEqual(oldFact, factJSON) {
					return Result{}, fmt.Errorf("fact %s conflicts with immutable revision", f.ID)
				}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return Result{}, err
			}
			reviewed, _ := time.Parse(time.RFC3339, f.ReviewedAt)
			basis, _ := json.Marshal(f.TestBasis)
			included, _ := json.Marshal(f.IncludedEnergy)
			_, err = tx.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, f.ID, p.ID, verified.PayloadHash, f.Kind, f.Value, f.Unit, basis, included, f.SourceKind, f.SourceURL, f.SourceHash, f.SourceHashBasis, f.SourceManifest, reviewed, f.Revision, factJSON)
			if err != nil {
				return Result{}, err
			}
		}
	}
	return result, tx.Commit(ctx)
}
func countFacts(p Payload) int {
	n := 0
	for _, product := range p.Products {
		n += len(product.Facts)
	}
	return n
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	leftDecoder := json.NewDecoder(bytes.NewReader(a))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(b))
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&x) != nil || rightDecoder.Decode(&y) != nil {
		return false
	}
	left, _ := json.Marshal(x)
	right, _ := json.Marshal(y)
	return bytes.Equal(left, right)
}
