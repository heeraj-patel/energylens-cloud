package catalog

import (
	"context"
	"strings"
)

// Link is the catalog product an appliance identity resolves to.
type Link struct{ ProductID, MatchQuality string }

// productGroup identifies one product for linking: duplicates share brand key,
// model, variant and region.
func productGroup(brandKey, model string, variant, region *string) string {
	return strings.Join([]string{CanonicalBrandKey(brandKey), normalizedModel(model), lowerOrEmpty(variant), lowerOrEmpty(region)}, "|")
}

func lowerOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(*s)
}

// FindLink returns the single exact product, else the single documented family
// product, for q. Duplicate products count as one and resolve to the newest
// active ENERGY STAR fact. Zero or several distinct products return false.
func FindLink(ctx context.Context, db catalogQueries, q Search) (Link, bool, error) {
	rows, err := db.Query(ctx, `SELECT p.id, `+brandKeySQL+`, p.full_model, p.variant, p.region
	    FROM catalog_products p JOIN active_catalog_facts f ON f.product_id=p.id AND f.kind='annual_energy'
	    WHERE (cardinality($1::text[])=0 OR `+brandKeySQL+` = ANY($1))
	      AND lower(btrim(p.full_model))=lower(btrim($2)) AND p.fuel=$3 AND p.match_quality='confirmed_exact'
	      AND (p.region IS NULL OR ($4::text IS NOT NULL AND lower(p.region)=lower($4)))
	      AND ($5::text IS NULL OR p.variant IS NULL OR lower(p.variant)=lower($5))
	    GROUP BY p.id
	    ORDER BY max(f.reviewed_at) FILTER (WHERE f.source_kind IN ('epa_exact','epa_family')) DESC NULLS LAST,
	             max(f.reviewed_at) DESC, p.id`, BrandKeys(q.Brand), q.Model, q.Fuel, q.Region, q.Variant)
	if err != nil {
		return Link{}, false, err
	}
	defer rows.Close()
	groups := map[string]string{}
	for rows.Next() {
		var id, brandKey, model string
		var variant, region *string
		if err = rows.Scan(&id, &brandKey, &model, &variant, &region); err != nil {
			return Link{}, false, err
		}
		group := productGroup(brandKey, model, variant, region)
		if _, ok := groups[group]; !ok {
			groups[group] = id
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Link{}, false, err
	}
	for _, id := range groups {
		if len(groups) == 1 {
			return Link{id, "confirmed_exact"}, true, nil
		}
	}
	if len(groups) > 1 {
		return Link{}, false, nil
	}
	candidates, err := (catalogReader{db}).familyCandidates(ctx, q, nil, 0)
	if err != nil {
		return Link{}, false, err
	}
	distinct := map[string]bool{}
	for _, c := range candidates {
		distinct[c.group] = true
	}
	if len(distinct) != 1 {
		return Link{}, false, nil
	}
	return Link{candidates[0].id, "documented_variant_family"}, true, nil
}
