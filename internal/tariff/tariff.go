package tariff

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound  = errors.New("home not found")
	ErrKeyReuse  = errors.New("idempotency key reused with different request")
	decimal      = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})(\.[0-9]{1,8})?$`)
	fixedDecimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,13})(\.[0-9]{1,4})?$`)
	currency     = regexp.MustCompile(`^[A-Z]{3}$`)
)

type Create struct {
	Fuel          string  `json:"fuel"`
	Rate          string  `json:"rate"`
	RateUnit      string  `json:"rate_unit"`
	Currency      string  `json:"currency"`
	RateKind      string  `json:"rate_kind"`
	SourceRef     string  `json:"source_ref"`
	SourceAsOf    string  `json:"source_as_of"`
	IsFallback    *bool   `json:"is_fallback"`
	FixedCharge   *string `json:"fixed_charge"`
	EffectiveFrom string  `json:"effective_from"`
	EffectiveTo   *string `json:"effective_to"`
}

type Tariff struct {
	ID       string `json:"id"`
	HomeID   string `json:"home_id"`
	Revision int64  `json:"revision"`
	Create
}

func (c Create) Valid(now time.Time) bool {
	if c.IsFallback == nil || !decimal.MatchString(c.Rate) || len(c.SourceRef) < 1 || len(c.SourceRef) > 2048 || strings.TrimSpace(c.SourceRef) == "" || !currency.MatchString(c.Currency) {
		return false
	}
	if c.FixedCharge != nil && !fixedDecimal.MatchString(*c.FixedCharge) {
		return false
	}
	switch c.Fuel {
	case "electricity":
		if c.RateUnit != "kWh" {
			return false
		}
	case "natural_gas":
		if c.RateUnit != "therm" {
			return false
		}
	case "propane", "fuel_oil":
		if c.RateUnit != "gallon" {
			return false
		}
	default:
		return false
	}
	if c.RateKind != "user_tariff" || *c.IsFallback {
		return false
	}
	asOf, e1 := time.Parse(time.RFC3339, c.SourceAsOf)
	from, e2 := time.Parse(time.RFC3339, c.EffectiveFrom)
	if e1 != nil || e2 != nil || asOf.After(now.Add(5*time.Minute)) {
		return false
	}
	var to time.Time
	if c.EffectiveTo != nil {
		var err error
		to, err = time.Parse(time.RFC3339, *c.EffectiveTo)
		if err != nil || !to.After(from) {
			return false
		}
	}
	// Historical imports may retain old source dates. A tariff that is still
	// effective needs a source checked within the last 18 months.
	if asOf.Before(now.AddDate(-1, -6, 0)) && (c.EffectiveTo == nil || to.After(now)) {
		return false
	}
	return true
}
