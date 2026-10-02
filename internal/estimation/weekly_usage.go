package estimation

import (
	"math/big"
	"time"
)

func weeklyUsageFraction(weekly string, start, end time.Time) (*big.Rat, error) {
	rate, err := number(weekly, true)
	if err != nil || rate.Cmp(big.NewRat(168, 1)) > 0 {
		return nil, ErrInvalidInput
	}
	days := int64(0)
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		days++
	}
	return new(big.Rat).Mul(rate, new(big.Rat).SetFrac64(days, 7)), nil
}

// ProjectWeeklyUsage turns a confirmed typical weekly frequency into expected
// usage for a home-calendar period. The result remains an estimate, including
// fractional expected cycles, and is never an observed load count.
func ProjectWeeklyUsage(weekly, startDate, endDate, timezone string) (string, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil || timezone == "" || timezone == "Local" {
		return "", ErrInvalidInput
	}
	start, err := day(startDate, location)
	if err != nil {
		return "", err
	}
	end, err := day(endDate, location)
	if err != nil || !end.After(start) || end.After(start.AddDate(10, 0, 0)) {
		return "", ErrInvalidInput
	}
	projection, err := weeklyUsageFraction(weekly, start, end)
	if err != nil {
		return "", err
	}
	return projection.FloatString(6), nil
}
