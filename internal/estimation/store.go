package estimation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PGRunStore struct{ Pool *pgxpool.Pool }

type applianceEvidence struct {
	ID        string
	Version   int64
	Category  string
	Brand     string
	Model     string
	Variant   *string
	Fuel      string
	Quality   string
	ProductID *string
}
type factEvidence struct {
	ID         string
	Revision   int64
	Value      string
	Count      int64
	Distinct   int64
	NonExact   int64
	Changes    int64
	SourceKind string
}
type rateEvidence struct {
	ID       string
	Revision int64
	Rate     string
	Currency string
	Kind     string
	Count    int64
}

func (s PGRunStore) ListRuns(ctx context.Context, owner, home string, appliance *string, now time.Time) ([]json.RawMessage, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var timezone string
	err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return nil, fmt.Errorf("invalid home timezone %q", timezone)
	}
	local := now.In(loc)
	start := fmt.Sprintf("%04d-%02d-01", local.Year(), local.Month())
	startTime := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	endTime := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	end := endTime.Format("2006-01-02")
	rows, err := tx.Query(ctx, `SELECT id,version,category,identity->>'brand',identity->>'full_model',identity->>'variant',identity->>'fuel',identity->>'match_quality',catalog_product_id
      FROM appliances WHERE home_id=$1 AND archived_at IS NULL AND ($2::uuid IS NULL OR id=$2)
      ORDER BY created_at,id`, home, appliance)
	if err != nil {
		return nil, err
	}
	items := []applianceEvidence{}
	for rows.Next() {
		var a applianceEvidence
		if err = rows.Scan(&a.ID, &a.Version, &a.Category, &a.Brand, &a.Model, &a.Variant, &a.Fuel, &a.Quality, &a.ProductID); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if appliance != nil && len(items) == 0 {
		return nil, ErrNotFound
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, a := range items {
		result, err := buildResolutionRun(ctx, tx, home, a, start, end, timezone, startTime, endTime, now)
		if err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
