package catalog

import "strings"

// Spellings of one brand beyond case and punctuation; mirrors datawiz BRAND_ALIASES.
var brandAliases = [][]string{
	{"LG", "LG Electronics", "LG SIGNATURE"},
	{"TP-Link", "TP Link", "TPLink"},
	{"Sansui", "SANSUI."},
	{"Loewe", "LOEWE."},
	{"BLACK+DECKER", "BLACK DECKER", "Black & Decker"},
}

// brandKeySQL is the stored brand_key, or the same key derived from brand for
// products that were imported without one.
const brandKeySQL = `COALESCE(NULLIF(p.product_json->>'brand_key',''), regexp_replace(lower(p.brand), '[^a-z0-9]+', '', 'g'))`

// BrandKey lowercases name and drops everything but ASCII letters and digits.
func BrandKey(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

func aliasGroup(name string) []string {
	key := BrandKey(name)
	for _, group := range brandAliases {
		for _, alias := range group {
			if BrandKey(alias) == key {
				return group
			}
		}
	}
	return nil
}

// BrandKeys returns every brand key that counts as name: its own and its alias
// group's. A blank name returns none, which matches any brand.
func BrandKeys(name string) []string {
	if strings.TrimSpace(name) == "" {
		return []string{}
	}
	keys := []string{BrandKey(name)}
	for _, alias := range aliasGroup(name) {
		if key := BrandKey(alias); key != keys[0] {
			keys = append(keys, key)
		}
	}
	return keys
}

// CanonicalBrandKey is the same for every spelling in an alias group.
func CanonicalBrandKey(name string) string {
	if group := aliasGroup(name); group != nil {
		return BrandKey(group[0])
	}
	return BrandKey(name)
}
