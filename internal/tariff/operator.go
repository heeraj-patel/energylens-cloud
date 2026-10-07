package tariff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSourceConflict = errors.New("official tariff source already imported with different terms")

// OfficialImport is a single home-scoped, manually reviewed rate. The source
// snapshot is supplied separately and checked before a revision is inserted.
type OfficialImport struct {
	OwnerUserID  string `json:"owner_user_id"`
	HomeID       string `json:"home_id"`
	SourceURL    string `json:"source_url"`
	SourceSHA256 string `json:"source_sha256"`
	Tariff       Create `json:"tariff"`
}

func (in OfficialImport) validate(snapshot []byte, actor string, now time.Time) (string, error) {
	if _, err := uuid.Parse(in.OwnerUserID); err != nil {
		return "", errors.New("valid owner_user_id required")
	}
	if _, err := uuid.Parse(in.HomeID); err != nil {
		return "", errors.New("valid home_id required")
	}
	if strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.ContainsAny(actor, "\r\n") {
		return "", errors.New("bounded operator identifier required")
	}
	u, err := url.Parse(in.SourceURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(in.SourceURL) > 1400 {
		return "", errors.New("HTTPS source URL required")
	}
	if len(snapshot) == 0 || len(snapshot) > 2<<20 {
		return "", errors.New("source snapshot must be 1 byte to 2 MiB")
	}
	digest := sha256.Sum256(snapshot)
	if in.SourceSHA256 != hex.EncodeToString(digest[:]) {
		return "", errors.New("source snapshot SHA-256 mismatch")
	}
	c := in.Tariff
	if c.SourceRef != "" || c.RateKind != "official_tariff" || c.IsFallback == nil || *c.IsFallback ||
		(c.Fuel != "electricity" && c.Fuel != "natural_gas") || c.Currency != "USD" ||
		!decimal.MatchString(c.Rate) || c.FixedCharge != nil && !fixedDecimal.MatchString(*c.FixedCharge) {
		return "", errors.New("invalid official electricity/gas marginal rate")
	}
	if c.Fuel == "electricity" && c.RateUnit != "kWh" || c.Fuel == "natural_gas" && c.RateUnit != "therm" {
		return "", errors.New("official rate fuel/unit mismatch")
	}
	asOf, e1 := time.Parse(time.RFC3339, c.SourceAsOf)
	from, e2 := time.Parse(time.RFC3339, c.EffectiveFrom)
	tooOld := asOf.Before(now.AddDate(-1, -6, 0))
	if c.Fuel == "electricity" {
		tooOld = !asOf.After(now.Add(-60 * 24 * time.Hour))
	}
	if e1 != nil || e2 != nil || c.SourceAsOf != normalizedTime(c.SourceAsOf) || c.EffectiveFrom != normalizedTime(c.EffectiveFrom) || asOf.After(now) || tooOld {
		return "", errors.New("official source date must be current and within its fuel-specific freshness window")
	}
	if c.EffectiveTo != nil {
		to, err := time.Parse(time.RFC3339, *c.EffectiveTo)
		if err != nil || !to.After(from) {
			return "", errors.New("invalid official rate effective interval")
		}
		if *c.EffectiveTo != normalizedTime(*c.EffectiveTo) {
			return "", errors.New("official effective_to must be UTC to the second")
		}
	}
	ref := fmt.Sprintf("%s sha256:%s reviewed_by:%s", in.SourceURL, in.SourceSHA256, actor)
	if len(ref) > 2048 {
		return "", errors.New("source attribution too long")
	}
	return ref, nil
}

// ImportOfficial is deliberately outside the customer HTTP surface. The
// operator must use a separate provisioning database credential.
func ImportOfficial(ctx context.Context, pool *pgxpool.Pool, in OfficialImport, snapshot []byte, actor string, now time.Time) (Tariff, error) {
	ref, err := in.validate(snapshot, actor, now.UTC())
	if err != nil {
		return Tariff{}, err
	}
	if pool == nil {
		return Tariff{}, errors.New("operator database required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Tariff{}, err
	}
	defer tx.Rollback(ctx)
	var home string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, in.HomeID, in.OwnerUserID).Scan(&home)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tariff{}, ErrNotFound
	}
	if err != nil {
		return Tariff{}, err
	}
	var existing Tariff
	sourcePrefix := fmt.Sprintf("%s sha256:%s reviewed_by:", in.SourceURL, in.SourceSHA256)
	existing, err = scanTariff(tx.QueryRow(ctx, selectTariff+` WHERE home_id=$1 AND fuel=$2 AND left(source_ref,length($3))=$3 ORDER BY revision DESC LIMIT 1`, home, in.Tariff.Fuel, sourcePrefix))
	if err == nil {
		c := in.Tariff
		if !sameDecimal(existing.Rate, c.Rate) || existing.RateUnit != c.RateUnit || existing.Currency != c.Currency || existing.RateKind != c.RateKind || existing.IsFallback == nil || *existing.IsFallback ||
			existing.SourceAsOf != normalizedTime(c.SourceAsOf) || existing.EffectiveFrom != normalizedTime(c.EffectiveFrom) || !sameOptionalTime(existing.EffectiveTo, c.EffectiveTo) || !sameOptionalDecimal(existing.FixedCharge, c.FixedCharge) {
			return Tariff{}, ErrSourceConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Tariff{}, err
	}
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM tariff_revisions WHERE home_id=$1 AND fuel=$2`, home, in.Tariff.Fuel).Scan(&revision); err != nil {
		return Tariff{}, err
	}
	c := in.Tariff
	c.SourceRef = ref
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,fixed_charge,effective_from,effective_to)
		VALUES($1,$2,$3,$4::numeric,$5,$6,$7,$8,$9,false,$10::numeric,$11,$12) RETURNING id`, home, revision, c.Fuel, c.Rate, c.RateUnit, c.Currency, c.RateKind, c.SourceRef, c.SourceAsOf, c.FixedCharge, c.EffectiveFrom, c.EffectiveTo).Scan(&id)
	if err != nil {
		return Tariff{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Tariff{}, err
	}
	return Tariff{ID: id, HomeID: home, Revision: revision, Create: c}, nil
}

func normalizedTime(raw string) string {
	t, _ := time.Parse(time.RFC3339, raw)
	return t.UTC().Format(time.RFC3339)
}

func sameOptionalTime(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return normalizedTime(*a) == normalizedTime(*b)
}

func sameDecimal(a, b string) bool {
	x, okX := new(big.Rat).SetString(a)
	y, okY := new(big.Rat).SetString(b)
	return okX && okY && x.Cmp(y) == 0
}

func sameOptionalDecimal(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameDecimal(*a, *b)
}
