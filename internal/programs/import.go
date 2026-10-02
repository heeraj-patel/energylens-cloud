package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReviewedImport is supplied by a human operator through an offline workflow.
// SourceBytes are the exact private source artifact, not extracted text.
type ReviewedImport struct {
	SourceKey         string
	SourceURL         string
	SourceBytes       []byte
	ContentType       string
	SnapshotRef       string
	ParserVersion     string
	AdministratorKey  string
	ProgramKey        string
	GeographyKey      string
	AdministratorName string
	Status            string
	Title             string
	BenefitKind       string
	BenefitTerms      json.RawMessage
	EligibilityTerms  json.RawMessage
	StackingTerms     json.RawMessage
	Dates             map[string]string // purchase_from/to, installation_from/to, reservation_from/to, application_from/to
	Evidence          map[string]string // reviewed field path -> nonempty locator in SourceBytes
	Reviewer          string
	ReviewNote        string
	ReviewExpiresAt   time.Time
	Applicability     *StructuredApplicability
}

var requiredEvidence = []string{"title", "status", "benefit_kind", "benefit_terms", "eligibility_terms", "stacking_terms"}
var dateFields = []string{"purchase_from", "purchase_to", "installation_from", "installation_to", "reservation_from", "reservation_to", "application_from", "application_to"}

func sourceHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Port() != "" || u.Fragment != "" || u.RawQuery != "" || strings.HasSuffix(u.Hostname(), ".") || strings.ToLower(u.Hostname()) != u.Hostname() {
		return "", errors.New("source URL must be a plain HTTPS URL on a registered host")
	}
	return u.Hostname(), nil
}

func object(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && len(value) > 0
}

func nullableDate(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func validateReviewedImport(in ReviewedImport, now time.Time) error {
	for _, s := range []string{in.SourceKey, in.SourceURL, in.ContentType, in.SnapshotRef, in.ParserVersion, in.AdministratorKey, in.ProgramKey, in.GeographyKey, in.AdministratorName, in.Title, in.Reviewer, in.ReviewNote} {
		if s == "" || strings.TrimSpace(s) != s {
			return errors.New("reviewed import has a blank or untrimmed field")
		}
	}
	if _, err := sourceHost(in.SourceURL); err != nil {
		return err
	}
	if len(in.SourceBytes) == 0 || len(in.SourceBytes) > 16<<20 {
		return errors.New("source artifact size is invalid")
	}
	if !in.ReviewExpiresAt.After(now) || in.ReviewExpiresAt.After(now.Add(90*24*time.Hour)) {
		return errors.New("review expiry must be within 90 days")
	}
	if err := validateStructured(in.Applicability); err != nil {
		return err
	}
	if in.Applicability != nil {
		for _, field := range []string{"country_code", "postal_codes", "category", "fuel", "provider_key", "provider_role", "requires_income", "requires_address", "additional_conditions"} {
			if strings.TrimSpace(in.Evidence["applicability."+field]) == "" {
				return fmt.Errorf("missing evidence for applicability.%s", field)
			}
		}
	}
	if !object(in.BenefitTerms) || !object(in.EligibilityTerms) || !object(in.StackingTerms) {
		return errors.New("reviewed terms must be nonempty JSON objects")
	}
	switch in.Status {
	case "proposed", "upcoming", "open", "waitlist", "exhausted", "suspended", "closed", "unknown":
	default:
		return errors.New("invalid status")
	}
	switch in.BenefitKind {
	case "rebate", "tax_credit", "bill_credit", "loan", "demand_response_reward", "other":
	default:
		return errors.New("invalid benefit kind")
	}
	allowed := map[string]bool{}
	for _, field := range requiredEvidence {
		allowed[field] = true
		if strings.TrimSpace(in.Evidence[field]) == "" {
			return fmt.Errorf("missing evidence for %s", field)
		}
	}
	for _, field := range dateFields {
		allowed[field] = true
		if value := in.Dates[field]; value != "" {
			parsed, err := time.Parse("2006-01-02", value)
			if err != nil || parsed.Format("2006-01-02") != value {
				return fmt.Errorf("invalid %s", field)
			}
			if strings.TrimSpace(in.Evidence[field]) == "" {
				return fmt.Errorf("missing evidence for %s", field)
			}
		}
	}
	for field, locator := range in.Evidence {
		if in.Applicability != nil && strings.HasPrefix(field, "applicability.") {
			allowed[field] = true
		}
		if !allowed[field] || strings.TrimSpace(locator) != locator || locator == "" {
			return fmt.Errorf("invalid evidence for %s", field)
		}
	}
	for field := range in.Dates {
		if !allowed[field] || in.Dates[field] == "" {
			return fmt.Errorf("invalid date field %s", field)
		}
	}
	return nil
}

// ImportReviewed persists one human-reviewed revision. Call only with an
// operator database credential; this package has no customer-facing route.
// Registration and review of official authorities remain operator decisions.
func ImportReviewed(ctx context.Context, db *pgxpool.Pool, in ReviewedImport) (uuid.UUID, error) {
	now := time.Now().UTC()
	if err := validateReviewedImport(in, now); err != nil {
		return uuid.Nil, err
	}
	host, _ := sourceHost(in.SourceURL)
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	var sourceID uuid.UUID
	var kind, parser string
	var hosts []string
	var enabled bool
	var maxBytes int64
	var termsReviewedAt, termsExpiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT id,source_kind,allowed_hosts,parser_version,enabled,terms_reviewed_at,terms_review_expires_at,max_response_bytes FROM program_sources WHERE source_key=$1 FOR UPDATE`, in.SourceKey).
		Scan(&sourceID, &kind, &hosts, &parser, &enabled, &termsReviewedAt, &termsExpiresAt, &maxBytes)
	if err != nil {
		return uuid.Nil, fmt.Errorf("registered source: %w", err)
	}
	allowed := false
	for _, h := range hosts {
		if h == host {
			allowed = true
		}
	}
	if !allowed || !enabled || kind == "directory" || parser != in.ParserVersion || termsReviewedAt.After(now) || !termsExpiresAt.After(in.ReviewExpiresAt) || int64(len(in.SourceBytes)) > maxBytes {
		return uuid.Nil, errors.New("source registration is not approved for this review")
	}
	digest := sha256.Sum256(in.SourceBytes)
	hash := hex.EncodeToString(digest[:])
	var snapshotID uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO program_source_snapshots(source_id,source_url,content_sha256,content_type,content_bytes,fetched_at,parser_version,snapshot_ref)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		sourceID, in.SourceURL, hash, in.ContentType, len(in.SourceBytes), now, in.ParserVersion, in.SnapshotRef).Scan(&snapshotID)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO program_source_snapshot_content(snapshot_id,content) VALUES($1,$2)`, snapshotID, in.SourceBytes); err != nil {
		return uuid.Nil, err
	}
	var programID, registeredSource uuid.UUID
	_, err = tx.Exec(ctx, `INSERT INTO public_programs(administrator_key,program_key,geography_key,administrator_name,source_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, in.AdministratorKey, in.ProgramKey, in.GeographyKey, in.AdministratorName, sourceID)
	if err != nil {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `SELECT id,source_id FROM public_programs WHERE administrator_key=$1 AND program_key=$2 AND geography_key=$3 FOR UPDATE`, in.AdministratorKey, in.ProgramKey, in.GeographyKey).Scan(&programID, &registeredSource)
	if err != nil {
		return uuid.Nil, err
	}
	if registeredSource != sourceID {
		return uuid.Nil, errors.New("program is bound to a different source")
	}
	var revisionID uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO public_program_revisions(program_id,revision,source_snapshot_id,lifecycle,status,title,benefit_kind,benefit_terms,eligibility_terms,stacking_terms,purchase_from,purchase_to,installation_from,installation_to,reservation_from,reservation_to,application_from,application_to,extracted_at,reviewed_at,review_expires_at,reviewed_by,review_note)
 VALUES($1,(SELECT coalesce(max(revision),0)+1 FROM public_program_revisions WHERE program_id=$1),$2,'reviewed',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17,$18,$19,$20) RETURNING id`,
		programID, snapshotID, in.Status, in.Title, in.BenefitKind, in.BenefitTerms, in.EligibilityTerms, in.StackingTerms,
		nullableDate(in.Dates["purchase_from"]), nullableDate(in.Dates["purchase_to"]), nullableDate(in.Dates["installation_from"]), nullableDate(in.Dates["installation_to"]), nullableDate(in.Dates["reservation_from"]), nullableDate(in.Dates["reservation_to"]), nullableDate(in.Dates["application_from"]), nullableDate(in.Dates["application_to"]), now, in.ReviewExpiresAt, in.Reviewer, in.ReviewNote).Scan(&revisionID)
	if err != nil {
		return uuid.Nil, err
	}
	if in.Applicability != nil {
		a := in.Applicability
		_, err = tx.Exec(ctx, `INSERT INTO reviewed_program_applicability(revision_id,country_code,postal_codes,category,fuel,provider_key,provider_role,requires_income,requires_address,additional_conditions) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, revisionID, a.CountryCode, a.PostalCodes, a.Category, a.Fuel, nullableString(a.ProviderKey), nullableString(a.ProviderRole), a.RequiresIncome, a.RequiresAddress, a.AdditionalConditions)
		if err != nil {
			return uuid.Nil, err
		}
	}
	for field, locator := range in.Evidence {
		if _, err = tx.Exec(ctx, `INSERT INTO public_program_evidence(revision_id,field_path,snapshot_id,locator) VALUES($1,$2,$3,$4)`, revisionID, field, snapshotID, locator); err != nil {
			return uuid.Nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return revisionID, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
