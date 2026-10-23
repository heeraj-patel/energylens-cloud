package householdenergy

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// habitCycle is one owner-tagged saved cycle with its reviewed reference energy.
type habitCycle struct {
	ApplianceID  string
	Start, End   time.Time
	MeasuredKWh  *big.Rat
	ReferenceKWh *big.Rat
}

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func floatRat(v float64) *big.Rat { return mustDecimal(strconv.FormatFloat(v, 'f', -1, 64)) }

// loadHabitCycles returns the owner's valid saved cycle evidence starting in p.
func loadHabitCycles(ctx context.Context, q rowsQuerier, owner, home string, p period, now time.Time) ([]habitCycle, error) {
	rows, err := q.Query(ctx, `SELECT appliance_id::text,start_at,end_at,measured_kwh,reference_kwh_per_cycle FROM dishwasher_cycle_evidence WHERE owner_user_id=$1 AND home_id=$2 AND invalidated_at IS NULL AND expires_at>$5 AND start_at>=$3 AND start_at<$4 ORDER BY appliance_id,start_at,created_at DESC,id`, owner, home, p.Start, p.End, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []habitCycle
	for rows.Next() {
		var c habitCycle
		var measured, reference float64
		if err = rows.Scan(&c.ApplianceID, &c.Start, &c.End, &measured, &reference); err != nil {
			return nil, err
		}
		c.MeasuredKWh, c.ReferenceKWh = floatRat(measured), floatRat(reference)
		out = append(out, c)
	}
	return out, rows.Err()
}

// habitsComponent prices each metered device's measured minus reference cycle
// energy over its saved cycles in the window, floored at 0 per device.
func habitsComponent(in componentInput) componentResult {
	res := componentResult{Devices: map[string]deviceAmount{}, Basis: "insufficient_data", Source: "none"}
	if in.Tariff == nil {
		return res
	}
	cycles := append([]habitCycle(nil), in.Habits...)
	sort.SliceStable(cycles, func(i, j int) bool {
		if cycles[i].ApplianceID != cycles[j].ApplianceID {
			return cycles[i].ApplianceID < cycles[j].ApplianceID
		}
		return cycles[i].Start.Before(cycles[j].Start)
	})
	sums := map[string]*big.Rat{}
	lastEnd := map[string]time.Time{}
	for _, c := range cycles {
		id := c.ApplianceID
		if !in.Metered[id] || c.MeasuredKWh == nil || c.ReferenceKWh == nil || !c.End.After(c.Start) || c.Start.Before(lastEnd[id]) {
			continue
		}
		// A cycle saved twice or overlapping an earlier saved cycle counts once.
		lastEnd[id] = c.End
		excessWh := new(big.Rat).Mul(new(big.Rat).Sub(c.MeasuredKWh, c.ReferenceKWh), big.NewRat(1000, 1))
		cost := priceIntervalRange(interval{Start: c.Start, End: c.End, Wh: excessWh}, c.Start, c.End, *in.Tariff)
		if cost == nil {
			continue
		}
		if sums[id] == nil {
			sums[id] = new(big.Rat)
		}
		sums[id].Add(sums[id], cost)
	}
	for id, v := range sums {
		if v.Sign() < 0 {
			v = new(big.Rat)
		}
		res.Devices[id] = deviceAmount{Amount: v, Basis: "measured"}
	}
	return res
}
