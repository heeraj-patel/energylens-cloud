// Package scenarioimport imports operator-signed reviewed action alternatives.
package scenarioimport

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

const schema = "energylens.reviewed-action-benefit-scenarios"
const domain = "energylens.reviewed-action-benefit-scenarios.v1\n"
const revisionDomain = "energylens.reviewed-action-benefit-scenarios.v2\n"

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var decimalPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type Scenario struct {
	ID                string `json:"id"`
	RuleID            string `json:"rule_id"`
	Category          string `json:"category"`
	Manufacturer      string `json:"manufacturer"`
	Model             string `json:"model"`
	Variant           string `json:"variant"`
	Fuel              string `json:"fuel"`
	BaselineFactID    string `json:"baseline_fact_id"`
	AlternativeKWh    string `json:"alternative_kwh"`
	UsageUnit         string `json:"usage_unit"`
	ServiceDefinition string `json:"service_definition"`
	ExclusionGroup    string `json:"exclusion_group"`
	SourceURL         string `json:"source_url"`
	SourceSHA256      string `json:"source_sha256"`
	ReviewSHA256      string `json:"review_sha256"`
	Reviewer          string `json:"reviewer"`
	ReviewedAt        string `json:"reviewed_at"`
	Revision          int64  `json:"revision"`
}
type Revision struct {
	Action        string  `json:"action"`
	PriorID       string  `json:"prior_id"`
	ReplacementID *string `json:"replacement_id"`
	Reason        string  `json:"reason"`
	Reviewer      string  `json:"reviewer"`
	ReviewedAt    string  `json:"reviewed_at"`
	ReviewSHA256  string  `json:"review_sha256"`
}
type Payload struct {
	Schema        string     `json:"schema"`
	SchemaVersion int        `json:"schema_version"`
	Scenarios     []Scenario `json:"scenarios"`
	Revision      *Revision  `json:"revision,omitempty"`
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
	Envelope                  Envelope
	PayloadHash, EnvelopeHash string
	artifact, publicPEM       []byte
}
type Result struct {
	PayloadSHA256 string `json:"payload_sha256"`
	Scenarios     int    `json:"scenarios"`
	Replayed      bool   `json:"replayed"`
}

func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&generic); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(generic); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func validUUID(s string) bool { id, err := uuid.Parse(s); return err == nil && id.String() == s }
func validTime(s string) bool {
	t, err := time.Parse(time.RFC3339, s)
	return err == nil && !t.IsZero()
}
func identity(s Scenario) string {
	return strings.Join([]string{s.RuleID, s.Category, s.Manufacturer, s.Model, s.Variant}, "\x00")
}
func validate(p Payload) error {
	if p.Schema != schema || (p.SchemaVersion != 1 && p.SchemaVersion != 2) {
		return errors.New("unsupported scenario publication")
	}
	if p.SchemaVersion == 1 && (len(p.Scenarios) == 0 || p.Revision != nil) {
		return errors.New("initial publication requires scenarios")
	}
	if p.SchemaVersion == 2 {
		r := p.Revision
		if r == nil || !validUUID(r.PriorID) || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 || strings.TrimSpace(r.Reviewer) == "" || !validTime(r.ReviewedAt) || !hashPattern.MatchString(r.ReviewSHA256) {
			return errors.New("invalid scenario revision")
		}
		if r.Action == "retract" {
			if len(p.Scenarios) != 0 || r.ReplacementID != nil {
				return errors.New("invalid scenario retraction")
			}
		} else if r.Action == "supersede" {
			if len(p.Scenarios) != 1 || r.ReplacementID == nil || !validUUID(*r.ReplacementID) || *r.ReplacementID != p.Scenarios[0].ID || *r.ReplacementID == r.PriorID {
				return errors.New("invalid scenario replacement")
			}
		} else {
			return errors.New("invalid scenario revision action")
		}
	}
	ids, identities := map[string]bool{}, map[string]bool{}
	for _, s := range p.Scenarios {
		if p.SchemaVersion == 1 && s.Revision != 1 {
			return errors.New("initial scenario revision must be one")
		}
		if !validUUID(s.ID) || ids[s.ID] || !validUUID(s.BaselineFactID) || s.RuleID == "" || s.Category == "" || s.Manufacturer == "" || s.Model == "" || s.Variant == "" || s.Fuel != "electricity" || s.ServiceDefinition == "" || s.ExclusionGroup == "" || s.Reviewer == "" || !validTime(s.ReviewedAt) || s.Revision < 1 {
			return errors.New("invalid scenario identity or review")
		}
		ids[s.ID] = true
		key := identity(s)
		if identities[key] {
			return errors.New("duplicate scenario identity")
		}
		identities[key] = true
		if s.UsageUnit != "kWh/use" && s.UsageUnit != "kWh/hour" {
			return errors.New("invalid scenario unit")
		}
		value, ok := new(big.Rat).SetString(s.AlternativeKWh)
		if !decimalPattern.MatchString(s.AlternativeKWh) || !ok || value.Sign() < 0 {
			return errors.New("invalid alternative energy")
		}
		u, err := url.Parse(s.SourceURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !hashPattern.MatchString(s.SourceSHA256) || !hashPattern.MatchString(s.ReviewSHA256) {
			return errors.New("invalid scenario provenance")
		}
	}
	return nil
}
func ParseAndVerify(artifact, publicPEM []byte) (Verified, error) {
	if len(artifact) == 0 || len(artifact) > 4<<20 {
		return Verified{}, errors.New("scenario artifact size is invalid")
	}
	block, rest := pem.Decode(publicPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return Verified{}, errors.New("invalid pinned key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Verified{}, err
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return Verified{}, errors.New("pinned key is not Ed25519")
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(artifact))
	dec.UseNumber()
	if err = dec.Decode(&generic); err != nil {
		return Verified{}, err
	}
	var extra any
	if err = dec.Decode(&extra); err == nil {
		return Verified{}, errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return Verified{}, err
	}
	normalized, err := canonical(generic)
	if err != nil || !bytes.Equal(artifact, normalized) {
		return Verified{}, errors.New("scenario envelope is not canonical JSON")
	}
	var env Envelope
	dec = json.NewDecoder(bytes.NewReader(artifact))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&env); err != nil {
		return Verified{}, err
	}
	if env.SchemaVersion != env.Payload.SchemaVersion || env.Algorithm != "Ed25519" {
		return Verified{}, errors.New("unsupported scenario envelope")
	}
	if err = validate(env.Payload); err != nil {
		return Verified{}, err
	}
	keyID := sha256.Sum256(block.Bytes)
	if env.KeyID != hex.EncodeToString(keyID[:]) {
		return Verified{}, errors.New("scenario signed by unauthorized key")
	}
	payload, err := canonical(env.Payload)
	if err != nil {
		return Verified{}, err
	}
	digest := sha256.Sum256(payload)
	if env.PayloadSHA256 != hex.EncodeToString(digest[:]) {
		return Verified{}, errors.New("scenario payload digest mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || len(env.Signature) != 86 {
		return Verified{}, errors.New("invalid scenario signature")
	}
	signingDomain := domain
	if env.SchemaVersion == 2 {
		signingDomain = revisionDomain
	}
	if !ed25519.Verify(key, append([]byte(signingDomain), payload...), signature) {
		return Verified{}, errors.New("scenario signature verification failed")
	}
	envelopeHash := sha256.Sum256(artifact)
	return Verified{env, hex.EncodeToString(digest[:]), hex.EncodeToString(envelopeHash[:]), bytes.Clone(artifact), bytes.Clone(publicPEM)}, nil
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
	if err != nil || rechecked.PayloadHash != verified.PayloadHash || rechecked.EnvelopeHash != verified.EnvelopeHash {
		return Result{}, errors.New("verified scenario publication changed before import")
	}
	verified = rechecked
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(209321, 71)`); err != nil {
		return Result{}, err
	}
	var oldKey, oldEnvelope string
	err = tx.QueryRow(ctx, `SELECT key_id,envelope_sha256 FROM reviewed_action_benefit_publications WHERE payload_sha256=$1`, verified.PayloadHash).Scan(&oldKey, &oldEnvelope)
	if err == nil {
		if oldKey != verified.Envelope.KeyID || oldEnvelope != verified.EnvelopeHash {
			return Result{}, errors.New("scenario replay differs from prior envelope")
		}
		return Result{verified.PayloadHash, len(verified.Envelope.Payload.Scenarios), true}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	p := verified.Envelope.Payload
	r := p.Revision
	if r != nil {
		var old Scenario
		err = tx.QueryRow(ctx, `SELECT id::text,rule_id,category,manufacturer,model,variant,revision FROM active_reviewed_action_benefit_scenarios WHERE id=$1`, r.PriorID).Scan(&old.ID, &old.RuleID, &old.Category, &old.Manufacturer, &old.Model, &old.Variant, &old.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, errors.New("scenario revision target is not active")
		}
		if err != nil {
			return Result{}, err
		}
		if r.Action == "supersede" && (identity(old) != identity(p.Scenarios[0]) || p.Scenarios[0].Revision != old.Revision+1) {
			return Result{}, errors.New("replacement changes scenario identity or revision")
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO reviewed_action_benefit_publications(payload_sha256,key_id,envelope_sha256,imported_by,scenario_count) VALUES($1,$2,$3,$4,$5)`, verified.PayloadHash, verified.Envelope.KeyID, verified.EnvelopeHash, actor, len(p.Scenarios)); err != nil {
		return Result{}, err
	}
	for _, s := range p.Scenarios {
		var activeID string
		err = tx.QueryRow(ctx, `SELECT id::text FROM active_reviewed_action_benefit_scenarios WHERE rule_id=$1 AND category=$2 AND manufacturer=$3 AND model=$4 AND variant=$5 LIMIT 1`, s.RuleID, s.Category, s.Manufacturer, s.Model, s.Variant).Scan(&activeID)
		if err == nil && (r == nil || activeID != r.PriorID) {
			return Result{}, errors.New("active scenario already owns identity")
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
		var baseValue, baseKind, baseUnit, baseService string
		err = tx.QueryRow(ctx, `SELECT value::text,measurement_kind,unit,COALESCE(cycle_definition,operation_mode) FROM active_reviewed_usage_facts WHERE id=$1 AND category=$2 AND manufacturer=$3 AND model=$4 AND variant=$5 AND fuel='electricity' AND match_quality='confirmed_exact' AND documented_scope=model AND matched_model=model AND source_kind IN ('oem_pdf','epa_row') AND included_energy->>'status'='documented' AND included_energy->'components'='["appliance_electricity"]'::jsonb AND ((measurement_kind='cycle_energy' AND unit='kWh/cycle' AND operation_mode IS NULL AND cycle_definition IS NOT NULL) OR (measurement_kind='hourly_energy' AND unit='kWh/hour' AND cycle_definition IS NULL AND operation_mode IS NOT NULL))`, s.BaselineFactID, s.Category, s.Manufacturer, s.Model, s.Variant).Scan(&baseValue, &baseKind, &baseUnit, &baseService)
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, errors.New("scenario baseline is not an active exact reviewed usage fact")
		}
		if err != nil {
			return Result{}, err
		}
		baseline, ok := new(big.Rat).SetString(baseValue)
		alt, _ := new(big.Rat).SetString(s.AlternativeKWh)
		if !ok || baseline.Sign() <= 0 || alt.Cmp(baseline) > 0 || baseService != s.ServiceDefinition || (s.UsageUnit == "kWh/use" && (baseKind != "cycle_energy" || baseUnit != "kWh/cycle")) || (s.UsageUnit == "kWh/hour" && (baseKind != "hourly_energy" || baseUnit != "kWh/hour")) {
			return Result{}, errors.New("scenario alternative does not match baseline service and unit")
		}
		reviewed, _ := time.Parse(time.RFC3339, s.ReviewedAt)
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_action_benefit_scenarios(id,publication_sha256,rule_id,category,manufacturer,model,variant,fuel,baseline_fact_id,alternative_kwh,usage_unit,service_definition,exclusion_group,source_url,source_sha256,review_sha256,reviewer,reviewed_at,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, s.ID, verified.PayloadHash, s.RuleID, s.Category, s.Manufacturer, s.Model, s.Variant, s.Fuel, s.BaselineFactID, s.AlternativeKWh, s.UsageUnit, s.ServiceDefinition, s.ExclusionGroup, s.SourceURL, s.SourceSHA256, s.ReviewSHA256, s.Reviewer, reviewed, s.Revision)
		if err != nil {
			return Result{}, fmt.Errorf("insert reviewed scenario: %w", err)
		}
	}
	if r != nil {
		reviewed, _ := time.Parse(time.RFC3339, r.ReviewedAt)
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_action_benefit_scenario_changes(prior_id,replacement_id,publication_sha256,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.PriorID, r.ReplacementID, verified.PayloadHash, r.Action, r.Reason, r.Reviewer, reviewed, r.ReviewSHA256)
		if err != nil {
			return Result{}, err
		}
	}
	return Result{verified.PayloadHash, len(p.Scenarios), false}, tx.Commit(ctx)
}
