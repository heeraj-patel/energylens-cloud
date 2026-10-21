package catalog

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PGStore struct{ Pool *pgxpool.Pool }

// SearchResearch exposes current model-subject observations even before a legacy
// catalog product has been imported. It does not create a product link.
func (s PGStore) SearchResearch(ctx context.Context, q Search) ([]datawizfacts.Fact, error) {
	if q.Brand == "" {
		return datawizfacts.DeviceByModel(ctx, s.Pool, q.Model)
	}
	return datawizfacts.Device(ctx, s.Pool, q.Brand, q.Model)
}

type catalogQueries interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type catalogReader struct{ db catalogQueries }

// SnapshotSearch and SnapshotGet bind the catalog body and its generation to
// one MVCC snapshot, including every fact lookup in a search response.
func (s PGStore) SnapshotSearch(ctx context.Context, q Search) ([]json.RawMessage, int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT generation FROM catalog_generation WHERE singleton=true`).Scan(&generation); err != nil {
		return nil, 0, err
	}
	products, err := (catalogReader{tx}).Search(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return products, generation, nil
}

func (s PGStore) SnapshotGet(ctx context.Context, id string) (json.RawMessage, int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT generation FROM catalog_generation WHERE singleton=true`).Scan(&generation); err != nil {
		return nil, 0, err
	}
	product, err := (catalogReader{tx}).Get(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return product, generation, nil
}

func (s PGStore) Search(ctx context.Context, q Search) ([]json.RawMessage, error) {
	return (catalogReader{s.Pool}).Search(ctx, q)
}

// Search never contacts an external provider. An absent region matches only
// region-neutral records; a specified region also includes neutral records.
func (s catalogReader) Search(ctx context.Context, q Search) ([]json.RawMessage, error) {
	rows, err := s.db.Query(ctx, `SELECT p.id FROM catalog_products p
	        WHERE (cardinality($1::text[])=0 OR `+brandKeySQL+` = ANY($1)) AND lower(p.full_model)=lower($2) AND ($3::text='' OR p.fuel=$3)
          AND (p.region IS NULL OR ($4::text IS NOT NULL AND lower(p.region)=lower($4)))
	      AND ($5::text IS NULL OR p.variant IS NULL OR lower(p.variant)=lower($5))
	    ORDER BY CASE p.match_quality WHEN 'confirmed_exact' THEN 0 ELSE 1 END,
	             CASE WHEN $4::text IS NOT NULL AND lower(p.region)=lower($4) THEN 0 ELSE 1 END, p.id
	    LIMIT 25`, BrandKeys(q.Brand), q.Model, q.Fuel, q.Region, q.Variant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	products := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		p, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	// A documented pattern is evidence of a possible family relationship only.
	// The reviewed annual-energy fact describes the representative product, so
	// candidate search results intentionally carry no facts.
	if len(products) == 25 || normalizedModel(q.Model) == "" {
		return products, nil
	}
	candidates, err := s.familyCandidates(ctx, q, ids, 25-len(products), false)
	if err != nil {
		return nil, err
	}
	candidateIDs := make([]string, len(candidates))
	for i, c := range candidates {
		candidateIDs[i] = c.id
	}
	for _, id := range candidateIDs {
		p, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		var candidate map[string]json.RawMessage
		if err = json.Unmarshal(p, &candidate); err != nil {
			return nil, err
		}
		candidate["facts"] = json.RawMessage(`[]`)
		delete(candidate, "research_facts")
		candidate["query_match"] = json.RawMessage(`"documented_family"`)
		if q.Variant == nil {
			candidate["variant_match"] = json.RawMessage(`"unresolved"`)
		} else if candidate["variant"] == nil || string(candidate["variant"]) == "null" {
			candidate["variant_match"] = json.RawMessage(`"unresolved"`)
		} else {
			candidate["variant_match"] = json.RawMessage(`"confirmed"`)
		}
		candidate["matched_model"], err = json.Marshal(strings.ToUpper(normalizedModel(q.Model)))
		if err != nil {
			return nil, err
		}
		p, err = json.Marshal(candidate)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, nil
}

func (s PGStore) Get(ctx context.Context, id string) (json.RawMessage, error) {
	return (catalogReader{s.Pool}).Get(ctx, id)
}

func (s catalogReader) Get(ctx context.Context, id string) (json.RawMessage, error) {
	var product []byte
	var facts []byte
	err := s.db.QueryRow(ctx, `SELECT p.product_json,
        COALESCE((SELECT jsonb_agg(f.fact_json || jsonb_build_object('source_fetched_at', f.source_fetched_at) ORDER BY f.reviewed_at DESC, f.id)
	                  FROM active_catalog_facts f WHERE f.product_id=p.id), '[]'::jsonb)
        FROM catalog_products p WHERE p.id=$1`, id).Scan(&product, &facts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(product, &result); err != nil {
		return nil, err
	}
	result["facts"] = facts
	var identity struct {
		Brand     string `json:"brand"`
		FullModel string `json:"full_model"`
	}
	if err = json.Unmarshal(product, &identity); err != nil {
		return nil, err
	}
	if identity.Brand != "" && identity.FullModel != "" {
		research, lookupErr := datawizfacts.Device(ctx, s.db, identity.Brand, identity.FullModel)
		if lookupErr != nil {
			return nil, lookupErr
		}
		result["research_facts"], err = json.Marshal(research)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(result)
}

type familyCandidate struct{ id, group string }

// familyCandidates lists documented_variant_family products whose reviewed
// pattern covers the requested model, skipping exclude. limit <= 0 is no cap.
// The length filter keeps patterns no longer than the model, as each * takes at least one character.
// With linking set, a non-electric product without a fact matches by its own model pattern.
func (s catalogReader) familyCandidates(ctx context.Context, q Search, exclude []string, limit int, linking bool) ([]familyCandidate, error) {
	if normalizedModel(q.Model) == "" {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT p.id, COALESCE(f.test_basis->>'matched_model', CASE WHEN f.id IS NULL THEN p.full_model END), `+brandKeySQL+`, p.full_model, p.variant, p.region
	    FROM catalog_products p LEFT JOIN active_catalog_facts f ON f.product_id=p.id
	      AND f.source_kind IN ('oem_family', 'other_official', 'epa_family', 'doe_ccd', 'oem_search', 'manual_research')
	    WHERE (cardinality($1::text[])=0 OR `+brandKeySQL+` = ANY($1)) AND ($2::text='' OR p.fuel=$2)
	      AND p.match_quality='documented_variant_family'
	      AND (f.id IS NOT NULL OR ($6::boolean AND p.fuel<>'electricity'))
	      AND (p.variant IS NULL OR $3::text IS NULL OR lower(p.variant)=lower($3))
	      AND (p.region IS NULL OR ($4::text IS NOT NULL AND lower(p.region)=lower($4)))
	      AND length(regexp_replace(upper(COALESCE(f.test_basis->>'matched_model', p.full_model)), '[^A-Z0-9*?]', '', 'g'))<=$5
	    ORDER BY CASE WHEN $4::text IS NOT NULL AND lower(p.region)=lower($4) THEN 0 ELSE 1 END,
	             p.id, f.reviewed_at DESC, f.id`, BrandKeys(q.Brand), q.Fuel, q.Variant, q.Region, len(normalizedModel(q.Model)), linking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		seen[id] = true
	}
	var found []familyCandidate
	for rows.Next() {
		var id, brandKey, model string
		var pattern, variant, region *string
		if err = rows.Scan(&id, &pattern, &brandKey, &model, &variant, &region); err != nil {
			return nil, err
		}
		if seen[id] || pattern == nil || !documentedPatternMatches(*pattern, q.Model) {
			continue
		}
		seen[id] = true
		found = append(found, familyCandidate{id, productGroup(brandKey, model, variant, region)})
		if limit > 0 && len(found) == limit {
			break
		}
	}
	// A bounded search may stop before exhausting rows. Release that connection
	// before the caller acquires another, including when the pool has one slot.
	rows.Close()
	return found, rows.Err()
}
