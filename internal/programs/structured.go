package programs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// StructuredApplicability is a reviewed projection of controlling source terms.
// Unknown requirements must be marked in AdditionalConditions.
type StructuredApplicability struct {
	CountryCode          string   `json:"country_code"`
	GeographyScope       string   `json:"geography_scope,omitempty"` // postal_code (default) or country
	PostalCodes          []string `json:"postal_codes"`
	Category             string   `json:"category"`
	Fuel                 string   `json:"fuel"`
	ProviderKey          string   `json:"provider_key,omitempty"`
	ProviderRole         string   `json:"provider_role,omitempty"`
	RequiresIncome       bool     `json:"requires_income"`
	RequiresAddress      bool     `json:"requires_address"`
	AdditionalConditions bool     `json:"additional_conditions"`
}

var usZIP = regexp.MustCompile(`^[0-9]{5}$`)

func validateStructured(s *StructuredApplicability) error {
	if s == nil {
		return nil
	}
	if s.CountryCode != "US" {
		return errors.New("structured applicability supports US locations only")
	}
	if s.GeographyScope != "" && s.GeographyScope != "postal_code" && s.GeographyScope != "country" {
		return errors.New("invalid geography scope")
	}
	if s.GeographyScope == "country" {
		if len(s.PostalCodes) != 0 {
			return errors.New("country scope cannot restrict postal codes")
		}
	} else if len(s.PostalCodes) == 0 || len(s.PostalCodes) > 10000 {
		return errors.New("postal scope required and bounded")
	}
	seen := map[string]bool{}
	for _, p := range s.PostalCodes {
		if !usZIP.MatchString(p) || seen[p] {
			return fmt.Errorf("invalid or duplicate postal code %q", p)
		}
		seen[p] = true
	}
	if s.Category == "" || strings.TrimSpace(s.Category) != s.Category || s.Fuel == "" || strings.TrimSpace(s.Fuel) != s.Fuel {
		return errors.New("category and fuel are required")
	}
	if (s.Category == "home") != (s.Fuel == "any") {
		return errors.New("home scope requires category home and fuel any")
	}
	if (s.ProviderKey == "") != (s.ProviderRole == "") {
		return errors.New("provider key and role must be paired")
	}
	if s.ProviderKey != "" && strings.TrimSpace(s.ProviderKey) != s.ProviderKey {
		return errors.New("invalid provider key")
	}
	if s.ProviderRole != "" {
		switch s.ProviderRole {
		case "electric_distributor", "electric_retailer", "gas_distributor", "gas_retailer", "other_fuel":
		default:
			return errors.New("invalid provider role")
		}
	}
	return nil
}
