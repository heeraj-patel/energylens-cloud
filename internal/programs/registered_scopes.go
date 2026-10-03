package programs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// registeredSourceScopes fans out only explicit operator-registered ZIP and
// category mappings. A source registration is a lead, never proof of provider
// service or program availability. Missing gas and jurisdiction sources remain
// a limitation in the resolver's coverage report.
func registeredSourceScopes(ctx context.Context, db *pgxpool.Pool, req DiscoveryRequest) ([]Scope, error) {
	rows, err := db.Query(ctx, `SELECT source_key,source_kind,authority_name,geography_coverage
		FROM program_sources WHERE geography_coverage->>'country_code'=$1
		AND geography_coverage->'postal_codes' ? $2
		AND (category_coverage='[]'::jsonb OR category_coverage ? $3)
		ORDER BY source_key LIMIT 129`, req.CountryCode, req.PostalCode, req.Category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := []Scope{}
	for rows.Next() {
		var key, kind, name string
		var raw []byte
		if err := rows.Scan(&key, &kind, &name, &raw); err != nil {
			return nil, err
		}
		var geo struct {
			ScopeKind    string `json:"scope_kind"`
			ProviderRole string `json:"provider_role"`
		}
		if err := json.Unmarshal(raw, &geo); err != nil {
			return nil, err
		}
		s := Scope{Kind: geo.ScopeKind, Key: key, Name: name, ProviderRole: geo.ProviderRole, SourceKey: key}
		validKind := kind == "utility" && s.Kind == "utility" || kind == "manufacturer" && s.Kind == "manufacturer" ||
			(kind == "government" || kind == "administrator") && (s.Kind == "city" || s.Kind == "county" || s.Kind == "state" || s.Kind == "federal" || s.Kind == "tribal")
		if !validKind || !validScope(s) {
			return nil, fmt.Errorf("registered source %q has invalid scope mapping", key)
		}
		scopes = append(scopes, s)
		if len(scopes) > 128 {
			return nil, fmt.Errorf("registered source fanout exceeds 128 scopes")
		}
	}
	return scopes, rows.Err()
}
