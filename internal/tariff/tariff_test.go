package tariff

import (
	"testing"
	"time"
)

func TestTariffValidation(t *testing.T) {
	now := time.Now().UTC()
	fallback := false
	fixed := "15.00"
	to := now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	valid := Create{Fuel: "electricity", Rate: "0.1588", RateUnit: "kWh", Currency: "USD", RateKind: "user_tariff", SourceRef: "bill 2026-09", SourceAsOf: now.Add(-24 * time.Hour).Format(time.RFC3339), IsFallback: &fallback, FixedCharge: &fixed, EffectiveFrom: now.Add(-24 * time.Hour).Format(time.RFC3339), EffectiveTo: &to}
	if !valid.Valid(now) {
		t.Fatal("valid user tariff rejected")
	}
	cases := map[string]func(*Create){
		"fuel unit":           func(c *Create) { c.Fuel = "natural_gas" },
		"negative rate":       func(c *Create) { c.Rate = "-0.1" },
		"binary float":        func(c *Create) { c.Rate = "1e3" },
		"fixed precision":     func(c *Create) { v := "1.12345"; c.FixedCharge = &v },
		"future source":       func(c *Create) { c.SourceAsOf = now.Add(time.Hour).Format(time.RFC3339) },
		"bad effective dates": func(c *Create) { v := now.Add(-48 * time.Hour).Format(time.RFC3339); c.EffectiveTo = &v },
		"forged official":     func(c *Create) { c.RateKind = "official_tariff" },
		"forged average":      func(c *Create) { c.RateKind = "regional_average" },
		"fallback user":       func(c *Create) { yes := true; c.IsFallback = &yes },
		"stale active user":   func(c *Create) { c.SourceAsOf = now.AddDate(-2, 0, 0).Format(time.RFC3339) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid
			mutate(&c)
			if c.Valid(now) {
				t.Fatalf("accepted %+v", c)
			}
		})
	}
	gas := valid
	gas.Fuel = "natural_gas"
	gas.RateUnit = "therm"
	if !gas.Valid(now) {
		t.Fatal("gas therm rejected")
	}
}

func TestElectricityTariffSourceExpiryBoundary(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fallback := false
	makeTariff := func(asOf time.Time) Create {
		return Create{Fuel: "electricity", Rate: "0.1588", RateUnit: "kWh", Currency: "USD", RateKind: "user_tariff", SourceRef: "bill", SourceAsOf: asOf.Format(time.RFC3339), IsFallback: &fallback, EffectiveFrom: now.Add(-24 * time.Hour).Format(time.RFC3339)}
	}
	tests := []struct {
		name string
		asOf time.Time
		want bool
	}{
		{name: "before expiry", asOf: now.Add(-ElectricityMaxAge + time.Second), want: true},
		{name: "exactly 90 days old", asOf: now.Add(-ElectricityMaxAge), want: false},
		{name: "older than 90 days", asOf: now.Add(-ElectricityMaxAge - time.Second), want: false},
		{name: "future source", asOf: now.Add(time.Second), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := makeTariff(tt.asOf).Valid(now); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}
