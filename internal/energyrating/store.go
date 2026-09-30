// Package energyrating persists sourced device ratings. It has no HTTP route;
// callers must explicitly choose when reviewed evidence may be distributed.
package energyrating

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrReviewedRatingProtected means a candidate upsert found an identity that
// is already reviewed; the reviewed row was left unchanged.
var ErrReviewedRatingProtected = errors.New("energyrating: reviewed rating is protected")

// Rating preserves one source tier and its provenance as a single record.
// Separate evidence sources are never combined into a synthetic rating.
type Rating struct {
	ID              int64
	Brand           string
	Model           string
	Category        string
	SourceKind      string
	AnnualKWh       *float64
	RatedWatts      *float64
	StandbyWatts    *float64
	KWhPerCycle     *float64
	UsageBasis      *string
	SourceRef       string
	SourceRowID     *string
	SourceFetchedAt *time.Time
	ContractVersion string
	Reviewed        bool
	ReviewedAt      *time.Time
}

// Candidate carries rating evidence before review. It intentionally has no
// review fields; only Store.Review can promote persisted evidence.
type Candidate struct {
	Brand           string
	Model           string
	Category        string
	SourceKind      string
	AnnualKWh       *float64
	RatedWatts      *float64
	StandbyWatts    *float64
	KWhPerCycle     *float64
	UsageBasis      *string
	SourceRef       string
	SourceRowID     *string
	SourceFetchedAt *time.Time
	ContractVersion string
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Store provides the intentionally small persistence seam for the rating
// catalog. FindReviewed returns no candidate until review is explicitly set.
type Store struct{ db queryRower }

func NewStore(db queryRower) *Store { return &Store{db: db} }

// FindReviewed returns the strongest reviewed rating for this exact model
// identity. Family entries are eligible only when the caller supplies that
// exact catalog model key; the result retains its source_kind so consumers
// can label family evidence honestly.
func (s *Store) FindReviewed(ctx context.Context, brand, model, category string) (Rating, bool, error) {
	const q = `
SELECT rating_id, brand, model, category, source_kind, annual_kwh::float8,
       rated_watts::float8, standby_watts::float8, kwh_per_cycle::float8,
       rating_usage_basis, source_ref, source_row_id, source_fetched_at,
       contract_version, reviewed, reviewed_at
FROM energy_rating_catalog
WHERE lower(brand) = lower($1) AND lower(model) = lower($2)
  AND category = $3 AND reviewed = TRUE
ORDER BY CASE source_kind
  WHEN 'epa_exact' THEN 0 WHEN 'oem_exact' THEN 1 ELSE 2 END,
  reviewed_at DESC, rating_id DESC
LIMIT 1`
	var r Rating
	err := s.db.QueryRow(ctx, q, brand, model, category).Scan(
		&r.ID, &r.Brand, &r.Model, &r.Category, &r.SourceKind, &r.AnnualKWh,
		&r.RatedWatts, &r.StandbyWatts, &r.KWhPerCycle, &r.UsageBasis,
		&r.SourceRef, &r.SourceRowID, &r.SourceFetchedAt,
		&r.ContractVersion, &r.Reviewed, &r.ReviewedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rating{}, false, nil
	}
	if err != nil {
		return Rating{}, false, err
	}
	return r, true, nil
}

// SaveCandidate stores unreviewed evidence. It cannot approve a candidate and
// it cannot replace evidence that has already been reviewed.
func (s *Store) SaveCandidate(ctx context.Context, r Candidate) error {
	const q = `
INSERT INTO energy_rating_catalog (
  brand, model, category, source_kind, annual_kwh, rated_watts,
  standby_watts, kwh_per_cycle, rating_usage_basis, source_ref,
  source_row_id, source_fetched_at, contract_version, reviewed, reviewed_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,FALSE,NULL)
ON CONFLICT (lower(brand), lower(model), source_kind) DO UPDATE SET
  category = EXCLUDED.category, annual_kwh = EXCLUDED.annual_kwh,
  rated_watts = EXCLUDED.rated_watts, standby_watts = EXCLUDED.standby_watts,
  kwh_per_cycle = EXCLUDED.kwh_per_cycle,
  rating_usage_basis = EXCLUDED.rating_usage_basis, source_ref = EXCLUDED.source_ref,
  source_row_id = EXCLUDED.source_row_id,
  source_fetched_at = EXCLUDED.source_fetched_at,
  contract_version = EXCLUDED.contract_version,
  reviewed = FALSE, reviewed_at = NULL, updated_at = now()
WHERE energy_rating_catalog.reviewed = FALSE`
	tag, err := s.db.Exec(ctx, q, r.Brand, r.Model, r.Category, r.SourceKind,
		r.AnnualKWh, r.RatedWatts, r.StandbyWatts, r.KWhPerCycle, r.UsageBasis,
		r.SourceRef, r.SourceRowID, r.SourceFetchedAt, r.ContractVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrReviewedRatingProtected
	}
	return nil
}

// Review is the only store operation that promotes a candidate. A zero time
// is rejected so reviewed evidence always has a durable review timestamp.
func (s *Store) Review(ctx context.Context, brand, model, sourceKind string, reviewedAt time.Time) (bool, error) {
	if reviewedAt.IsZero() {
		return false, errors.New("energyrating: review timestamp is required")
	}
	const q = `UPDATE energy_rating_catalog
SET reviewed = TRUE, reviewed_at = $4, updated_at = now()
WHERE lower(brand) = lower($1) AND lower(model) = lower($2)
  AND source_kind = $3 AND reviewed = FALSE`
	tag, err := s.db.Exec(ctx, q, brand, model, sourceKind, reviewedAt.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
