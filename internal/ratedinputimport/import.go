// Package ratedinputimport verifies signed, operator-reviewed nameplate input facts.
package ratedinputimport

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

const schema = "energylens.reviewed-rated-input-specs"
const domain = "energylens.reviewed-rated-input-specs.v1\n"
const revisionDomain = "energylens.reviewed-rated-input-specs.v2\n"
const maxArtifactBytes = 4 << 20

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var decimalPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type Fact struct {
	ID                    string `json:"id"`
	Category              string `json:"category"`
	Manufacturer          string `json:"manufacturer"`
	Model                 string `json:"model"`
	SourceKind            string `json:"source_kind"`
	MatchedModel          string `json:"matched_model"`
	RatedInputWatts       string `json:"rated_input_watts"`
	Unit                  string `json:"unit"`
	SourceURL             string `json:"source_url"`
	ManufacturerSourceURL string `json:"manufacturer_source_url"`
	SourceSHA256          string `json:"source_sha256"`
	ReviewSHA256          string `json:"review_sha256"`
	Reviewer              string `json:"reviewer"`
	ReviewedAt            int64  `json:"reviewed_at"`
	UsageBasis            string `json:"usage_basis"`
	SourceManifest        string `json:"source_manifest"`
}
type Revision struct {
	ID                string  `json:"id"`
	Action            string  `json:"action"`
	PriorFactID       string  `json:"prior_fact_id"`
	ReplacementFactID *string `json:"replacement_fact_id"`
	Reason            string  `json:"reason"`
	Reviewer          string  `json:"reviewer"`
	ReviewedAt        string  `json:"reviewed_at"`
	ReviewSHA256      string  `json:"review_sha256"`
}
type Payload struct {
	Schema        string    `json:"schema"`
	SchemaVersion int       `json:"schema_version"`
	Facts         []Fact    `json:"facts"`
	Revision      *Revision `json:"revision,omitempty"`
}
type Envelope struct {
	SchemaVersion int     `json:"schema_version"`
	Algorithm     string  `json:"algorithm"`
	Payload       Payload `json:"payload"`
	PayloadSHA256 string  `json:"payload_sha256"`
	KeyID         string  `json:"key_id"`
	Signature     string  `json:"signature"`
}
type Verified struct {
	Envelope     Envelope
	PayloadHash  string
	EnvelopeHash string
	artifact     []byte
	publicPEM    []byte
}
type Result struct {
	PayloadSHA256 string `json:"payload_sha256"`
	Facts         int    `json:"facts"`
	Replayed      bool   `json:"replayed"`
}

func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&generic); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err = e.Encode(generic); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func ParseAndVerify(artifact, publicPEM []byte) (Verified, error) {
	if len(artifact) == 0 || len(artifact) > maxArtifactBytes {
		return Verified{}, errors.New("rated-input artifact size is invalid")
	}
	block, rest := pem.Decode(publicPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return Verified{}, errors.New("invalid pinned key")
	}
	public, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Verified{}, err
	}
	key, ok := public.(ed25519.PublicKey)
	if !ok {
		return Verified{}, errors.New("pinned key is not Ed25519")
	}
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
	normalized, err := canonical(generic)
	if err != nil || !bytes.Equal(artifact, normalized) {
		return Verified{}, errors.New("rated-input envelope is not canonical JSON")
	}
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(artifact))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&env); err != nil {
		return Verified{}, err
	}
	version := env.Payload.SchemaVersion
	if env.SchemaVersion != version || (version != 1 && version != 2) || env.Algorithm != "Ed25519" || env.Payload.Schema != schema {
		return Verified{}, errors.New("unsupported rated-input publication")
	}
	keyID := sha256.Sum256(block.Bytes)
	if env.KeyID != hex.EncodeToString(keyID[:]) {
		return Verified{}, errors.New("publication signed by unauthorized key")
	}
	payload, err := canonical(env.Payload)
	if err != nil {
		return Verified{}, err
	}
	payloadHash := sha256.Sum256(payload)
	if env.PayloadSHA256 != hex.EncodeToString(payloadHash[:]) {
		return Verified{}, errors.New("publication payload digest mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || len(env.Signature) != 86 {
		return Verified{}, errors.New("invalid publication signature")
	}
	signingDomain := domain
	if version == 2 {
		signingDomain = revisionDomain
	}
	if !ed25519.Verify(key, append([]byte(signingDomain), payload...), signature) {
		return Verified{}, errors.New("publication signature verification failed")
	}
	if err = validate(env.Payload); err != nil {
		return Verified{}, err
	}
	envelopeHash := sha256.Sum256(artifact)
	return Verified{Envelope: env, PayloadHash: hex.EncodeToString(payloadHash[:]), EnvelopeHash: hex.EncodeToString(envelopeHash[:]), artifact: bytes.Clone(artifact), publicPEM: bytes.Clone(publicPEM)}, nil
}

func validate(p Payload) error {
	if p.SchemaVersion == 1 {
		if len(p.Facts) == 0 || p.Revision != nil {
			return errors.New("invalid initial rated-input publication")
		}
	} else {
		r := p.Revision
		if r == nil || !validUUID(r.ID) || !validUUID(r.PriorFactID) || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 || strings.TrimSpace(r.Reviewer) == "" || !hashPattern.MatchString(r.ReviewSHA256) {
			return errors.New("invalid rated-input revision provenance")
		}
		if _, err := time.Parse(time.RFC3339, r.ReviewedAt); err != nil {
			return errors.New("invalid rated-input revision time")
		}
		if r.Action == "retract" {
			if len(p.Facts) != 0 || r.ReplacementFactID != nil {
				return errors.New("invalid rated-input retraction")
			}
		} else if r.Action == "supersede" {
			if len(p.Facts) != 1 || r.ReplacementFactID == nil || *r.ReplacementFactID != p.Facts[0].ID || !validUUID(*r.ReplacementFactID) {
				return errors.New("invalid rated-input replacement")
			}
		} else {
			return errors.New("invalid rated-input revision action")
		}
	}
	ids, identities := map[string]bool{}, map[string]bool{}
	for _, f := range p.Facts {
		if !validUUID(f.ID) || ids[f.ID] || strings.TrimSpace(f.Category) == "" || strings.TrimSpace(f.Manufacturer) == "" || strings.TrimSpace(f.Model) == "" || f.SourceKind != "oem_rated_input" || strings.TrimSpace(f.MatchedModel) == "" || f.Unit != "W" || f.ReviewedAt <= 0 || strings.TrimSpace(f.Reviewer) == "" || strings.TrimSpace(f.UsageBasis) == "" {
			return errors.New("invalid rated-input fact")
		}
		ids[f.ID] = true
		identity := strings.ToLower(strings.Join([]string{f.Category, modelKey(f.Manufacturer), modelKey(f.Model)}, "|"))
		if identities[identity] {
			return errors.New("duplicate rated-input identity")
		}
		identities[identity] = true
		watts, ok := new(big.Rat).SetString(f.RatedInputWatts)
		if !decimalPattern.MatchString(f.RatedInputWatts) || !ok || watts.Cmp(big.NewRat(1, 10)) < 0 || watts.Cmp(big.NewRat(50000, 1)) > 0 {
			return errors.New("rated input must be bounded watts")
		}
		for _, raw := range []string{f.SourceURL, f.ManufacturerSourceURL} {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return errors.New("invalid rated-input source URL")
			}
		}
		if !hashPattern.MatchString(f.SourceSHA256) || !hashPattern.MatchString(f.ReviewSHA256) || strings.Contains(f.SourceManifest, "..") || strings.ContainsAny(f.SourceManifest, `/\\`) || f.SourceManifest == "" {
			return errors.New("invalid rated-input provenance")
		}
	}
	return nil
}

func validUUID(s string) bool { id, err := uuid.Parse(s); return err == nil && id.String() == s }
func modelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
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
	if !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o022 != 0 {
		return Verified{}, errors.New("pinned key file is insecure")
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

func Import(ctx context.Context, pool *pgxpool.Pool, verified Verified, actor string) (Result, error) {
	if strings.TrimSpace(actor) == "" {
		return Result{}, errors.New("imported-by is required")
	}
	rechecked, err := ParseAndVerify(verified.artifact, verified.publicPEM)
	if err != nil || verified.PayloadHash != rechecked.PayloadHash || verified.EnvelopeHash != rechecked.EnvelopeHash {
		return Result{}, errors.New("verified rated-input publication changed before import")
	}
	verified = rechecked
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(209321, 70)`); err != nil {
		return Result{}, err
	}
	var oldKey, oldEnvelope string
	err = tx.QueryRow(ctx, `SELECT key_id,envelope_sha256 FROM reviewed_rated_input_publications WHERE payload_sha256=$1`, verified.PayloadHash).Scan(&oldKey, &oldEnvelope)
	if err == nil {
		if oldKey != verified.Envelope.KeyID || oldEnvelope != verified.EnvelopeHash {
			return Result{}, errors.New("publication replay differs from prior envelope")
		}
		return Result{PayloadSHA256: verified.PayloadHash, Facts: len(verified.Envelope.Payload.Facts), Replayed: true}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	r := verified.Envelope.Payload.Revision
	if r != nil {
		var oldCategory, oldManufacturer, oldModel string
		err = tx.QueryRow(ctx, `SELECT category,manufacturer,model FROM active_reviewed_rated_input_facts WHERE id=$1`, r.PriorFactID).Scan(&oldCategory, &oldManufacturer, &oldModel)
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, errors.New("revision target is not active")
		}
		if err != nil {
			return Result{}, err
		}
		if r.Action == "supersede" {
			f := verified.Envelope.Payload.Facts[0]
			if !strings.EqualFold(oldCategory, f.Category) || modelKey(oldManufacturer) != modelKey(f.Manufacturer) || modelKey(oldModel) != modelKey(f.Model) {
				return Result{}, errors.New("replacement changes rated-input identity")
			}
		}
	}
	result := Result{PayloadSHA256: verified.PayloadHash, Facts: len(verified.Envelope.Payload.Facts)}
	if _, err = tx.Exec(ctx, `INSERT INTO reviewed_rated_input_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$2,$3,$4,$5)`, verified.PayloadHash, verified.Envelope.KeyID, verified.EnvelopeHash, actor, result.Facts); err != nil {
		return Result{}, err
	}
	for _, f := range verified.Envelope.Payload.Facts {
		var activeID string
		err = tx.QueryRow(ctx, `SELECT id::text FROM active_reviewed_rated_input_facts WHERE lower(category)=lower($1) AND regexp_replace(upper(manufacturer),'[^A-Z0-9]','','g')=$2 AND regexp_replace(upper(model),'[^A-Z0-9]','','g')=$3 LIMIT 1`, f.Category, modelKey(f.Manufacturer), modelKey(f.Model)).Scan(&activeID)
		if err == nil && (r == nil || activeID != r.PriorFactID) {
			return Result{}, errors.New("active reviewed rated-input fact already owns identity")
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
		var oldPublication string
		err = tx.QueryRow(ctx, `SELECT publication_sha256 FROM reviewed_rated_input_facts WHERE id=$1`, f.ID).Scan(&oldPublication)
		if err == nil {
			return Result{}, fmt.Errorf("fact %s already belongs to publication %s", f.ID, oldPublication)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
		factJSON, _ := json.Marshal(f)
		reviewed := time.Unix(f.ReviewedAt, 0).UTC()
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_rated_input_facts(id,publication_sha256,category,manufacturer,model,source_kind,matched_model,rated_input_watts,unit,source_url,manufacturer_source_url,source_sha256,review_sha256,reviewer,reviewed_at,usage_basis,source_manifest,fact_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, f.ID, verified.PayloadHash, f.Category, f.Manufacturer, f.Model, f.SourceKind, f.MatchedModel, f.RatedInputWatts, f.Unit, f.SourceURL, f.ManufacturerSourceURL, f.SourceSHA256, f.ReviewSHA256, f.Reviewer, reviewed, f.UsageBasis, f.SourceManifest, factJSON)
		if err != nil {
			return Result{}, err
		}
	}
	if r != nil {
		reviewed, _ := time.Parse(time.RFC3339, r.ReviewedAt)
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_rated_input_revisions(id,publication_sha256,prior_fact_id,replacement_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.ID, verified.PayloadHash, r.PriorFactID, r.ReplacementFactID, r.Action, r.Reason, r.Reviewer, reviewed, r.ReviewSHA256)
		if err != nil {
			return Result{}, err
		}
	}
	return result, tx.Commit(ctx)
}
