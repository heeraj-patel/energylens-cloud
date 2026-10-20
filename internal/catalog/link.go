package catalog

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"
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

// hasWattsOrStandbySQL holds when exact product p has an active rated-input or
// standby usage fact, matched by normalized brand and model like Datawiz.
const hasWattsOrStandbySQL = `(EXISTS (SELECT 1 FROM reviewed_rated_input_facts u
	      WHERE u.unit='W' AND p.match_quality='confirmed_exact'
	        AND regexp_replace(upper(u.manufacturer),'[^A-Z0-9]','','g')=regexp_replace(upper(p.brand),'[^A-Z0-9]','','g')
	        AND regexp_replace(upper(u.model),'[^A-Z0-9]','','g')=regexp_replace(upper(p.full_model),'[^A-Z0-9]','','g')
	        AND NOT EXISTS (SELECT 1 FROM reviewed_rated_input_revisions r WHERE r.prior_fact_id=u.id))
	    OR EXISTS (SELECT 1 FROM reviewed_usage_facts u
	      WHERE u.measurement_kind='standby_power' AND u.unit='W' AND u.fuel='electricity' AND u.match_quality='confirmed_exact'
	        AND p.match_quality='confirmed_exact'
	        AND regexp_replace(upper(u.manufacturer),'[^A-Z0-9]','','g')=regexp_replace(upper(p.brand),'[^A-Z0-9]','','g')
	        AND regexp_replace(upper(u.model),'[^A-Z0-9]','','g')=regexp_replace(upper(p.full_model),'[^A-Z0-9]','','g')
	        AND NOT EXISTS (SELECT 1 FROM reviewed_usage_fact_revisions r WHERE r.prior_fact_id=u.id)))`

// FindLink returns the single exact product, else the single documented family
// product, else a prefix-trim family product, for q. Duplicate products count
// as one and resolve to the newest active ENERGY STAR fact. An exact product
// links with any active fact; a product with an annual fact outranks one with
// only rated-input or standby facts. Family and trim links need annual facts.
func FindLink(ctx context.Context, db catalogQueries, q Search) (Link, bool, error) {
	rows, err := db.Query(ctx, `SELECT p.id, `+brandKeySQL+`, p.full_model, p.variant, p.region
	    FROM catalog_products p LEFT JOIN active_catalog_facts f ON f.product_id=p.id AND f.kind='annual_energy'
	    WHERE (f.id IS NOT NULL OR `+hasWattsOrStandbySQL+`)
	      AND (cardinality($1::text[])=0 OR `+brandKeySQL+` = ANY($1))
	      AND lower(btrim(p.full_model))=lower(btrim($2)) AND p.fuel=$3 AND p.match_quality='confirmed_exact'
	      AND (p.region IS NULL OR ($4::text IS NOT NULL AND lower(p.region)=lower($4)))
	      AND ($5::text IS NULL OR p.variant IS NULL OR lower(p.variant)=lower($5))
	    GROUP BY p.id
	    ORDER BY (count(f.id)>0) DESC,
	             max(f.reviewed_at) FILTER (WHERE f.source_kind IN ('epa_exact','epa_family')) DESC NULLS LAST,
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
	if len(distinct) > 1 {
		return Link{}, false, nil
	}
	if len(distinct) == 1 {
		return Link{candidates[0].id, "documented_variant_family"}, true, nil
	}
	return findTrimLink(ctx, db, q)
}

const (
	minTrimLength = 6
	maxKWhSpread  = 1.15
)

var nonModelChars = regexp.MustCompile(`[^A-Za-z0-9*]`)

type trimCandidate struct {
	id, group, model string
	kwh              float64
	epa              bool
	reviewed         time.Time
}

// findTrimLink links the same-brand products whose model starts with q's model
// minus k trailing characters, for the smallest k that has any. Several
// products link only when their annual kWh agree; computers and TVs never trim.
func findTrimLink(ctx context.Context, db catalogQueries, q Search) (Link, bool, error) {
	if c := strings.ToLower(strings.TrimSpace(q.Category)); c == "computer" || c == "tv" {
		return Link{}, false, nil
	}
	model := normalizedModel(q.Model)
	if len(model) < minTrimLength {
		return Link{}, false, nil
	}
	var re strings.Builder
	re.WriteByte('^')
	for i := 0; i < minTrimLength; i++ {
		re.WriteString("[" + model[i:i+1] + "*]")
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (p.id) p.id, `+brandKeySQL+`, p.full_model, p.variant, p.region,
	           f.value::float8, f.source_kind IN ('epa_exact','epa_family'), f.reviewed_at
	    FROM catalog_products p JOIN active_catalog_facts f ON f.product_id=p.id AND f.kind='annual_energy'
	    WHERE (cardinality($1::text[])=0 OR `+brandKeySQL+` = ANY($1)) AND p.fuel=$2
	      AND (p.region IS NULL OR ($3::text IS NOT NULL AND lower(p.region)=lower($3)))
	      AND ($4::text IS NULL OR p.variant IS NULL OR lower(p.variant)=lower($4))
	      AND regexp_replace(upper(p.full_model), '[^A-Z0-9*]', '', 'g') ~ $5
	    ORDER BY p.id, f.source_kind IN ('epa_exact','epa_family') DESC, f.reviewed_at DESC, f.id`,
		BrandKeys(q.Brand), q.Fuel, q.Region, q.Variant, re.String())
	if err != nil {
		return Link{}, false, err
	}
	defer rows.Close()
	var all []trimCandidate
	for rows.Next() {
		var c trimCandidate
		var brandKey, full string
		var variant, region *string
		if err = rows.Scan(&c.id, &brandKey, &full, &variant, &region, &c.kwh, &c.epa, &c.reviewed); err != nil {
			return Link{}, false, err
		}
		c.group = productGroup(brandKey, full, variant, region)
		c.model = strings.ToUpper(nonModelChars.ReplaceAllString(full, ""))
		all = append(all, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Link{}, false, err
	}
	sort.Slice(all, func(i, j int) bool { return newerFact(all[i], all[j]) })
	for n := len(model); n >= minTrimLength; n-- {
		var found []trimCandidate
		seen := map[string]bool{}
		for _, c := range all {
			if !seen[c.group] && trimPrefixMatches(model[:n], c.model) {
				seen[c.group] = true
				found = append(found, c)
			}
		}
		if len(found) > 0 {
			return medianLink(found)
		}
	}
	return Link{}, false, nil
}

// newerFact orders ENERGY STAR facts first, then newest, then by id.
func newerFact(a, b trimCandidate) bool {
	if a.epa != b.epa {
		return a.epa
	}
	if !a.reviewed.Equal(b.reviewed) {
		return a.reviewed.After(b.reviewed)
	}
	return a.id < b.id
}

func trimPrefixMatches(prefix, model string) bool {
	if len(model) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if model[i] != '*' && model[i] != prefix[i] {
			return false
		}
	}
	return true
}

func medianLink(found []trimCandidate) (Link, bool, error) {
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].kwh != found[j].kwh {
			return found[i].kwh < found[j].kwh
		}
		return newerFact(found[i], found[j])
	})
	lo, hi := found[0].kwh, found[len(found)-1].kwh
	if len(found) > 1 && (lo <= 0 && hi > lo || lo > 0 && hi/lo > maxKWhSpread) {
		return Link{}, false, nil
	}
	return Link{found[(len(found)-1)/2].id, "documented_variant_family"}, true, nil
}

// SendableLink returns the match quality to send for an appliance's linked
// product. An exact link must match the appliance brand and model; a family
// link need not.
func SendableLink(brand, model string, p LinkedProduct) (string, bool) {
	switch p.MatchQuality {
	case "documented_variant_family":
		return p.MatchQuality, true
	case "confirmed_exact":
		if CanonicalBrandKey(brand) == CanonicalBrandKey(p.Brand) && normalizedModel(model) == normalizedModel(p.FullModel) {
			return p.MatchQuality, true
		}
	}
	return "", false
}

// LinkedProduct is the catalog product row an appliance links to.
type LinkedProduct struct{ Brand, FullModel, MatchQuality string }
