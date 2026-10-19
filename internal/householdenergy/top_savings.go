package householdenergy

import (
	"context"
	"math/big"
	"sort"
	"time"
)

// DeviceWaste is one device's trailing-30-day waste cost normalized to a month.
type DeviceWaste struct {
	ApplianceID   string
	DeviceID      string
	Name          string
	MonthlyWaste  *big.Rat
	Currency      string
	EvidenceBasis string
	WindowStart   time.Time
	WindowEnd     time.Time
}

// DeviceMonthlyWaste returns each device's waste cost over the trailing 30
// days, scaled to 30 days of coverage. Devices without a known waste cost are omitted.
func (s Service) DeviceMonthlyWaste(ctx context.Context, owner, home string) ([]DeviceWaste, error) {
	c, err := s.compute(ctx, owner, home, "day", "trailing_30_days")
	if err != nil {
		return nil, err
	}
	days := len(c.buckets)
	if days == 0 {
		return nil, nil
	}
	type sum struct {
		row     DeviceWaste
		total   *big.Rat
		invalid bool
	}
	sums := map[string]*sum{}
	order := []string{}
	for _, b := range c.buckets {
		for _, d := range b.DeviceAllocations {
			key := d.ApplianceID
			if key == "" {
				key = "device:" + d.DeviceID
			}
			waste, ok := d.Allocation.WasteCost.(money)
			if !ok {
				continue
			}
			amount, ok := new(big.Rat).SetString(waste.Amount)
			if !ok {
				continue
			}
			cur := sums[key]
			if cur == nil {
				cur = &sum{row: DeviceWaste{ApplianceID: d.ApplianceID, DeviceID: d.DeviceID, Currency: waste.Currency, EvidenceBasis: d.Allocation.EvidenceBasis}, total: new(big.Rat)}
				sums[key] = cur
				order = append(order, key)
			}
			if cur.row.Currency != waste.Currency {
				cur.invalid = true
				continue
			}
			if cur.row.Name == "" && d.Name != nil {
				cur.row.Name = *d.Name
			}
			cur.total.Add(cur.total, amount)
		}
	}
	sort.Strings(order)
	scale := big.NewRat(30, int64(days))
	out := make([]DeviceWaste, 0, len(order))
	for _, key := range order {
		cur := sums[key]
		if cur.invalid {
			continue
		}
		cur.row.MonthlyWaste = new(big.Rat).Mul(cur.total, scale)
		cur.row.WindowStart, cur.row.WindowEnd = c.base.Period.Start, c.base.Period.End
		out = append(out, cur.row)
	}
	return out, nil
}
