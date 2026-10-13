package ratedinputapi

import (
	"context"
	"encoding/json"
	"time"

	"energylens.local/cloud/internal/datawizfacts"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) Research(ctx context.Context, scope Scope) ([]datawizfacts.Fact, error) {
	all, err := datawizfacts.Device(ctx, s.Pool, scope.Manufacturer, scope.Model)
	if err != nil {
		return nil, err
	}
	out := []datawizfacts.Fact{}
	for _, f := range all {
		if f.Field != "energy_rating" {
			continue
		}
		var v struct {
			Unit   string `json:"unit"`
			Metric string `json:"metric"`
		}
		if json.Unmarshal(f.Value, &v) == nil && v.Unit == "W" && (v.Metric == "rated_input" || v.Metric == "rated_input_watts" || v.Metric == "rated_watts" || v.Metric == "nameplate_input" || v.Metric == "input_power") {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s PGStore) Find(ctx context.Context, scope Scope) ([]Fact, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text,category,manufacturer,model,source_kind,matched_model,rated_input_watts::text,unit,source_url,manufacturer_source_url,source_sha256,review_sha256,reviewer,reviewed_at,usage_basis,source_manifest,publication_sha256
 FROM active_reviewed_rated_input_facts
 WHERE lower(category)=lower($1)
 AND regexp_replace(upper(manufacturer),'[^A-Z0-9]','','g')=regexp_replace(upper($2),'[^A-Z0-9]','','g')
 AND regexp_replace(upper(model),'[^A-Z0-9]','','g')=regexp_replace(upper($3),'[^A-Z0-9]','','g')
 ORDER BY id LIMIT 2`, scope.Category, scope.Manufacturer, scope.Model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make([]Fact, 0, 1)
	for rows.Next() {
		var fact Fact
		var reviewedAt time.Time
		if err = rows.Scan(&fact.ID, &fact.Category, &fact.Manufacturer, &fact.Model, &fact.SourceKind, &fact.MatchedModel, &fact.RatedInputWatts, &fact.Unit, &fact.SourceURL, &fact.ManufacturerSourceURL, &fact.SourceSHA256, &fact.ReviewSHA256, &fact.Reviewer, &reviewedAt, &fact.UsageBasis, &fact.SourceManifest, &fact.PublicationSHA256); err != nil {
			return nil, err
		}
		fact.ReviewedAt = reviewedAt.UTC().Format(time.RFC3339Nano)
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}
