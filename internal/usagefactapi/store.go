package usagefactapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PGStore struct{ Pool *pgxpool.Pool }

// Find is bounded to the three supported measurement kinds. The exact identity
// and both documented model fields must agree before a fact can leave Cloud.
func (s PGStore) Find(ctx context.Context, scope Scope) ([]Fact, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text,measurement_kind,value::text,unit,test_basis,cycle_definition,operation_mode,included_energy,
 source_kind,source_url,source_sha256,source_page_index,dataset_id,row_identifier,field_name,metadata_sha256,
	 review_sha256,reviewed_at,publication_sha256
 FROM active_reviewed_usage_facts
 WHERE category=$1 AND lower(manufacturer)=lower($2) AND lower(model)=lower($3) AND fuel=$4 AND lower(variant)=lower($5)
 AND match_quality='confirmed_exact' AND lower(documented_scope)=lower($3) AND lower(matched_model)=lower($3)
 AND measurement_kind IN ('cycle_energy','hourly_energy','standby_power')
 ORDER BY measurement_kind,id LIMIT 4`, scope.Category, scope.Manufacturer, scope.Model, scope.Fuel, scope.Variant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make([]Fact, 0, 3)
	seen := map[string]bool{}
	for rows.Next() {
		var f Fact
		var included []byte
		var reviewedAt time.Time
		if err = rows.Scan(&f.ID, &f.MeasurementKind, &f.Value, &f.Unit, &f.TestBasis, &f.CycleDefinition, &f.OperationMode, &included, &f.SourceKind, &f.SourceURL, &f.SourceSHA256, &f.SourcePageIndex, &f.DatasetID, &f.RowIdentifier, &f.FieldName, &f.MetadataSHA256, &f.ReviewSHA256, &reviewedAt, &f.PublicationSHA256); err != nil {
			return nil, err
		}
		if seen[f.MeasurementKind] || len(facts) == 3 {
			return nil, errors.New("ambiguous reviewed usage facts")
		}
		seen[f.MeasurementKind] = true
		if !json.Valid(included) {
			return nil, errors.New("invalid reviewed usage inclusion")
		}
		f.IncludedEnergy = json.RawMessage(included)
		f.ReviewedAt = reviewedAt.UTC().Format(time.RFC3339Nano)
		facts = append(facts, f)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}
