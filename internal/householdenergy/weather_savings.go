package householdenergy

import (
	"context"
	"math/big"
)

// WeatherDevice is one appliance's inputs to a weather tip estimate.
type WeatherDevice struct {
	Metered bool
	// MonthlyShiftSaving is the measured peak shift saving per 30 days; nil
	// unless the device is a metered shiftable one on a TOU tariff.
	MonthlyShiftSaving *big.Rat
}

// WeatherSavings are the trailing-30-day figures behind weather tip estimates,
// each scaled to 30 days of coverage. A nil cost means there is no figure.
type WeatherSavings struct {
	Devices          map[string]WeatherDevice
	ShiftCurrency    string
	ElectricMonthly  *big.Rat
	ElectricCurrency string
	GasMonthly       *big.Rat
	GasCurrency      string
}

// WeatherDeviceSavings reads the home once over the trailing 30 days.
func (s Service) WeatherDeviceSavings(ctx context.Context, owner, home string) (WeatherSavings, error) {
	c, err := s.compute(ctx, owner, home, "day", "trailing_30_days")
	if err != nil {
		return WeatherSavings{}, err
	}
	out := WeatherSavings{Devices: map[string]WeatherDevice{}}
	days := len(c.buckets)
	if days == 0 {
		return out, nil
	}
	scale := big.NewRat(30, int64(days))
	monthly := func(v any) (*big.Rat, string) {
		m, ok := v.(money)
		if !ok {
			return nil, ""
		}
		amount, ok := new(big.Rat).SetString(m.Amount)
		if !ok {
			return nil, ""
		}
		return amount.Mul(amount, scale), m.Currency
	}
	out.ElectricMonthly, out.ElectricCurrency = monthly(c.totals.Cost)
	out.GasMonthly, out.GasCurrency = monthly(c.totals.GasCost)
	if c.component == nil {
		return out, nil
	}
	for id := range c.component.Metered {
		out.Devices[id] = WeatherDevice{Metered: true}
	}
	if c.component.Tariff == nil {
		return out, nil
	}
	out.ShiftCurrency = c.component.Tariff.Currency
	for id, amount := range peakComponent(*c.component).Devices {
		if amount.Amount == nil {
			continue
		}
		d := out.Devices[id]
		d.MonthlyShiftSaving = new(big.Rat).Mul(amount.Amount, scale)
		out.Devices[id] = d
	}
	return out, nil
}
