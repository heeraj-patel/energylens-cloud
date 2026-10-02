// Package usagefactimport verifies operator-signed per-cycle and per-hour
// evidence. It is deliberately separate from annual catalog import and HTTP.
package usagefactimport

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

const domain = "energylens.reviewed-usage-facts.v1\n"
const maxArtifactBytes = 16 << 20

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var decimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type IncludedEnergy struct {
	Status       string   `json:"status"`
	Components   []string `json:"components"`
	EvidenceText string   `json:"evidence_text"`
}
type Fact struct {
	ID              string         `json:"id"`
	Category        string         `json:"category"`
	Manufacturer    string         `json:"manufacturer"`
	Model           string         `json:"model"`
	Fuel            string         `json:"fuel"`
	Variant         string         `json:"variant"`
	MatchQuality    string         `json:"match_quality"`
	DocumentedScope string         `json:"documented_scope"`
	MatchedModel    string         `json:"matched_model"`
	MeasurementKind string         `json:"measurement_kind"`
	Value           string         `json:"value"`
	Unit            string         `json:"unit"`
	TestBasis       string         `json:"test_basis"`
	CycleDefinition *string        `json:"cycle_definition"`
	OperationMode   *string        `json:"operation_mode"`
	IncludedEnergy  IncludedEnergy `json:"included_energy"`
	SourceKind      string         `json:"source_kind"`
	SourceURL       string         `json:"source_url"`
	SourceSHA256    string         `json:"source_sha256"`
	SourcePageIndex *int           `json:"source_page_index"`
	DatasetID       *string        `json:"dataset_id"`
	RowIdentifier   *string        `json:"row_identifier"`
	FieldName       *string        `json:"field_name"`
	MetadataSHA256  *string        `json:"metadata_sha256"`
	ReviewSHA256    string         `json:"review_sha256"`
	Reviewer        string         `json:"reviewer"`
	ReviewedAt      string         `json:"reviewed_at"`
	SourceManifest  string         `json:"source_manifest"`
}
type Payload struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	Facts         []Fact `json:"facts"`
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
	EnvelopeHash string
	PayloadHash  string
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
func decodeStrict(raw []byte, out any) error {
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
func presentFields(raw []byte) error {
	// Go's decoder accepts omitted struct fields. Require the exact published
	// field set, including explicit nulls, before accepting a signature.
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		return err
	}
	if err := exactKeys(outer, "schema_version", "algorithm", "payload", "payload_sha256", "key_id", "signature"); err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(outer["payload"], &payload); err != nil {
		return err
	}
	if err := exactKeys(payload, "schema", "schema_version", "facts"); err != nil {
		return err
	}
	var facts []map[string]json.RawMessage
	if err := json.Unmarshal(payload["facts"], &facts); err != nil {
		return err
	}
	for _, fact := range facts {
		if err := exactKeys(fact, "id", "category", "manufacturer", "model", "fuel", "variant", "match_quality", "documented_scope", "matched_model", "measurement_kind", "value", "unit", "test_basis", "cycle_definition", "operation_mode", "included_energy", "source_kind", "source_url", "source_sha256", "source_page_index", "dataset_id", "row_identifier", "field_name", "metadata_sha256", "review_sha256", "reviewer", "reviewed_at", "source_manifest"); err != nil {
			return err
		}
		var included map[string]json.RawMessage
		if err := json.Unmarshal(fact["included_energy"], &included); err != nil {
			return err
		}
		if err := exactKeys(included, "status", "components", "evidence_text"); err != nil {
			return err
		}
	}
	return nil
}
func exactKeys(m map[string]json.RawMessage, names ...string) error {
	if len(m) != len(names) {
		return errors.New("publication fields differ from schema")
	}
	for _, name := range names {
		if _, ok := m[name]; !ok {
			return errors.New("publication field missing: " + name)
		}
	}
	return nil
}
func ParseAndVerify(artifact, publicPEM []byte) (Verified, error) {
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
		return Verified{}, errors.New("envelope is not canonical JSON")
	}
	if err = presentFields(artifact); err != nil {
		return Verified{}, err
	}
	var env Envelope
	if err = decodeStrict(artifact, &env); err != nil {
		return Verified{}, err
	}
	if env.SchemaVersion != 1 || env.Algorithm != "Ed25519" || env.Payload.Schema != "energylens.reviewed-usage-facts" || env.Payload.SchemaVersion != 1 {
		return Verified{}, errors.New("unsupported publication format")
	}
	keyID := sha256.Sum256(block.Bytes)
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
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || len(env.Signature) != 86 || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != env.Signature {
		return Verified{}, errors.New("invalid signature encoding")
	}
	if !ed25519.Verify(key, append([]byte(domain), payload...), sig) {
		return Verified{}, errors.New("signature verification failed")
	}
	if err = validate(env.Payload); err != nil {
		return Verified{}, err
	}
	envelopeHash := sha256.Sum256(artifact)
	return Verified{Envelope: env, EnvelopeHash: hex.EncodeToString(envelopeHash[:]), PayloadHash: env.PayloadSHA256, artifact: bytes.Clone(artifact), publicPEM: bytes.Clone(publicPEM)}, nil
}

func validUUID(s string) bool         { id, err := uuid.Parse(s); return err == nil && id.String() == s }
func nonblank(s string) bool          { return s != "" && strings.TrimSpace(s) == s }
func optionalNonblank(s *string) bool { return s == nil || nonblank(*s) }
func modelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var documentedPatternRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9*._ /-]*[A-Za-z0-9]$`)
var requestedModelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ /()-]*[A-Za-z0-9)]$`)

func validScope(f Fact) bool {
	scope, model, matched := modelKey(f.DocumentedScope), modelKey(f.Model), modelKey(f.MatchedModel)
	if scope == "" || model == "" || matched == "" || len(f.Model) > 100 || !requestedModelRE.MatchString(f.Model) {
		return false
	}
	if f.MatchQuality == "confirmed_exact" {
		return scope == model && matched == model && !strings.Contains(f.DocumentedScope, "*")
	}
	if strings.Contains(f.DocumentedScope, "*") {
		if f.MatchQuality != "documented_variant_family" || len(f.DocumentedScope) < 5 || len(f.DocumentedScope) > 80 || !documentedPatternRE.MatchString(f.DocumentedScope) || strings.Contains(f.DocumentedScope, ".*") || strings.Count(f.DocumentedScope, "*") > 2 || len(scope) < 5 || !strings.Contains(strings.ToUpper(f.MatchedModel), strings.ToUpper(f.DocumentedScope)) {
			return false
		}
		pattern := strings.ToUpper(f.DocumentedScope)
		var normalized strings.Builder
		for _, r := range pattern {
			if r == '*' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				normalized.WriteRune(r)
			}
		}
		pattern = normalized.String()
		if len(pattern) != len(model) {
			return false
		}
		for i := range pattern {
			if pattern[i] != '*' && pattern[i] != model[i] {
				return false
			}
		}
		return true
	}
	return len(scope) >= 5 && strings.Contains(model, scope) && strings.Contains(matched, scope)
}
func validate(p Payload) error {
	if len(p.Facts) == 0 {
		return errors.New("publication has no facts")
	}
	ids := map[string]bool{}
	identities := map[string]bool{}
	for _, f := range p.Facts {
		if !validUUID(f.ID) || ids[f.ID] || !nonblank(f.Category) || !nonblank(f.Manufacturer) || !nonblank(f.Model) || !nonblank(f.Variant) || f.Fuel != "electricity" || !nonblank(f.DocumentedScope) || !nonblank(f.MatchedModel) {
			return errors.New("invalid or duplicate usage identity")
		}
		ids[f.ID] = true
		identity := strings.ToLower(strings.Join([]string{f.Category, f.Manufacturer, modelKey(f.Model), f.Fuel, f.Variant, f.MeasurementKind}, "|"))
		if identities[identity] {
			return errors.New("duplicate usage fact identity")
		}
		identities[identity] = true
		if f.MatchQuality != "confirmed_exact" && f.MatchQuality != "documented_variant_family" {
			return errors.New("invalid match quality")
		}
		if !validScope(f) {
			return errors.New("documented model scope differs")
		}
		if f.MeasurementKind != "cycle_energy" && f.MeasurementKind != "hourly_energy" || !nonblank(f.TestBasis) {
			return errors.New("invalid measurement basis")
		}
		if f.MeasurementKind == "cycle_energy" && (f.Unit != "kWh/cycle" || f.CycleDefinition == nil || !nonblank(*f.CycleDefinition) || f.OperationMode != nil) || f.MeasurementKind == "hourly_energy" && (f.Unit != "kWh/hour" || f.OperationMode == nil || !nonblank(*f.OperationMode) || f.CycleDefinition != nil) {
			return errors.New("measurement unit or mode differs")
		}
		value, ok := new(big.Rat).SetString(f.Value)
		if !decimalRE.MatchString(f.Value) || !ok || value.Sign() <= 0 {
			return errors.New("usage value must be positive decimal")
		}
		if f.IncludedEnergy.Status != "documented" || !nonblank(f.IncludedEnergy.EvidenceText) || len(f.IncludedEnergy.Components) == 0 {
			return errors.New("included energy is undocumented")
		}
		seenComponent := map[string]bool{}
		for _, component := range f.IncludedEnergy.Components {
			if component != "appliance_electricity" && component != "water_heating_electricity" && component != "standby_electricity" || seenComponent[component] {
				return errors.New("invalid included energy component")
			}
			seenComponent[component] = true
		}
		if !seenComponent["appliance_electricity"] {
			return errors.New("included energy omits appliance electricity")
		}
		source, err := url.Parse(f.SourceURL)
		if err != nil || source.Scheme != "https" || source.Host == "" || source.User != nil || !hashRE.MatchString(f.SourceSHA256) || !hashRE.MatchString(f.ReviewSHA256) || !nonblank(f.Reviewer) || !nonblank(f.SourceManifest) || strings.Contains(f.SourceManifest, "..") || strings.ContainsAny(f.SourceManifest, `/\\`) {
			return errors.New("invalid source or review provenance")
		}
		if f.SourceKind == "oem_pdf" {
			if f.SourcePageIndex == nil || *f.SourcePageIndex < 0 || f.DatasetID != nil || f.RowIdentifier != nil || f.FieldName != nil || f.MetadataSHA256 != nil {
				return errors.New("invalid OEM provenance")
			}
		} else if f.SourceKind == "epa_row" {
			if f.MatchQuality != "confirmed_exact" || f.SourcePageIndex != nil || f.DatasetID == nil || !nonblank(*f.DatasetID) || f.RowIdentifier == nil || !nonblank(*f.RowIdentifier) || f.FieldName == nil || !nonblank(*f.FieldName) || f.MetadataSHA256 == nil || !hashRE.MatchString(*f.MetadataSHA256) {
				return errors.New("invalid EPA provenance")
			}
		} else {
			return errors.New("invalid source kind")
		}
		reviewed, err := time.Parse(time.RFC3339, f.ReviewedAt)
		if err != nil || reviewed.IsZero() || reviewed.After(time.Now().Add(5*time.Minute)) || !optionalNonblank(f.CycleDefinition) || !optionalNonblank(f.OperationMode) {
			return errors.New("invalid review time or mode")
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
	if !nonblank(actor) {
		return Result{}, errors.New("imported-by is required")
	}
	// Keep the signed artifact as the source of truth even for an in-process caller.
	rechecked, err := ParseAndVerify(verified.artifact, verified.publicPEM)
	if err != nil || !bytes.Equal(verified.artifact, rechecked.artifact) || verified.PayloadHash != rechecked.PayloadHash || verified.EnvelopeHash != rechecked.EnvelopeHash {
		return Result{}, errors.New("verified publication changed before import")
	}
	verified = rechecked
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(209321, 33)`); err != nil {
		return Result{}, err
	}
	var oldKey, oldEnvelope string
	err = tx.QueryRow(ctx, `SELECT key_id,envelope_sha256 FROM reviewed_usage_publications WHERE payload_sha256=$1`, verified.PayloadHash).Scan(&oldKey, &oldEnvelope)
	if err == nil {
		if oldKey != verified.Envelope.KeyID || oldEnvelope != verified.EnvelopeHash {
			return Result{}, errors.New("publication replay differs from prior envelope")
		}
		return Result{PayloadSHA256: verified.PayloadHash, Facts: len(verified.Envelope.Payload.Facts), Replayed: true}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	result := Result{PayloadSHA256: verified.PayloadHash, Facts: len(verified.Envelope.Payload.Facts)}
	_, err = tx.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$2,$3,$4,$5)`, verified.PayloadHash, verified.Envelope.KeyID, verified.EnvelopeHash, actor, result.Facts)
	if err != nil {
		return Result{}, err
	}
	for _, f := range verified.Envelope.Payload.Facts {
		factJSON, _ := json.Marshal(f)
		var oldPublication string
		err = tx.QueryRow(ctx, `SELECT publication_sha256 FROM reviewed_usage_facts WHERE id=$1`, f.ID).Scan(&oldPublication)
		if err == nil {
			return Result{}, fmt.Errorf("fact %s already belongs to publication %s", f.ID, oldPublication)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
		reviewed, _ := time.Parse(time.RFC3339, f.ReviewedAt)
		included, _ := json.Marshal(f.IncludedEnergy)
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_usage_facts(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,operation_mode,included_energy,source_kind,source_url,source_sha256,source_page_index,dataset_id,row_identifier,field_name,metadata_sha256,review_sha256,reviewer,reviewed_at,source_manifest,fact_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::numeric,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`, f.ID, verified.PayloadHash, f.Category, f.Manufacturer, f.Model, f.Fuel, f.Variant, f.MatchQuality, f.DocumentedScope, f.MatchedModel, f.MeasurementKind, f.Value, f.Unit, f.TestBasis, f.CycleDefinition, f.OperationMode, included, f.SourceKind, f.SourceURL, f.SourceSHA256, f.SourcePageIndex, f.DatasetID, f.RowIdentifier, f.FieldName, f.MetadataSHA256, f.ReviewSHA256, f.Reviewer, reviewed, f.SourceManifest, factJSON)
		if err != nil {
			return Result{}, err
		}
	}
	return result, tx.Commit(ctx)
}
