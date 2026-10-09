package householdenergy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Each measured instant must have exactly one fresh, nonfallback tariff. The
// selected schedule may contain adjacent revisions; a gap or overlap makes the
// complete response cost unavailable while leaving its energy intact.
func readEffectiveTariff(ctx context.Context, tx pgx.Tx, home string, now time.Time, items []interval) (string, int64, *marginalTariff, error) {
	var latestID string
	var latestRevision int64
	err := tx.QueryRow(ctx, `SELECT id::text,revision FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity' ORDER BY revision DESC LIMIT 1`, home).Scan(&latestID, &latestRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		latestID = ""
	} else if err != nil {
		return "", 0, nil, err
	}
	if len(items) == 0 {
		return latestID, latestRevision, nil, nil
	}
	first, last := items[0].Start, items[0].End
	for i, item := range items {
		if item.Unavailable || item.Fuel == nil || *item.Fuel != "electricity" || item.Wh == nil {
			return latestID, latestRevision, nil, nil
		}
		first, last = minTime(first, item.Start), maxTime(last, item.End)
		for j := 0; j < i; j++ {
			if item.Start.Before(items[j].End) && items[j].Start.Before(item.End) {
				return latestID, latestRevision, nil, nil
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,revision,rate::text,currency,effective_from,effective_to,source_as_of
 FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity' AND rate_unit='kWh'
 AND rate_kind IN ('user_tariff','official_tariff') AND NOT is_fallback
 AND effective_from<$3 AND (effective_to IS NULL OR effective_to>$2)
 ORDER BY effective_from,revision`, home, first, last)
	if err != nil {
		return latestID, latestRevision, nil, err
	}
	segments := []marginalTariff{}
	for rows.Next() {
		var v marginalTariff
		var rate string
		if err = rows.Scan(&v.ID, &v.Revision, &rate, &v.Currency, &v.From, &v.To, &v.SourceAsOf); err != nil {
			rows.Close()
			return latestID, latestRevision, nil, err
		}
		var ok bool
		v.Rate, ok = new(big.Rat).SetString(rate)
		if !ok {
			rows.Close()
			return latestID, latestRevision, nil, fmt.Errorf("invalid persisted tariff rate")
		}
		segments = append(segments, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return latestID, latestRevision, nil, err
	}
	selected := selectTariffSchedule(now, items, segments)
	return latestID, latestRevision, selected, nil
}

func selectTariffSchedule(now time.Time, items []interval, segments []marginalTariff) *marginalTariff {
	currency := ""
	for _, item := range items {
		cuts := tariffCuts(item.Start, item.End, segments)
		for i := 0; i+1 < len(cuts); i++ {
			selected := tariffAt(cuts[i], segments)
			if selected == nil || selected.SourceAsOf.After(now) || !selected.SourceAsOf.After(now.Add(-1440*time.Hour)) {
				return nil
			}
			if currency == "" {
				currency = selected.Currency
			} else if currency != selected.Currency {
				return nil
			}
		}
	}
	if currency == "" {
		return nil
	}
	return &marginalTariff{Currency: currency, Segments: segments}
}

func tariffCuts(start, end time.Time, segments []marginalTariff) []time.Time {
	cuts := []time.Time{start, end}
	for _, t := range segments {
		if t.From.After(start) && t.From.Before(end) {
			cuts = append(cuts, t.From)
		}
		if t.To != nil && t.To.After(start) && t.To.Before(end) {
			cuts = append(cuts, *t.To)
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	unique := cuts[:0]
	for _, cut := range cuts {
		if len(unique) == 0 || !cut.Equal(unique[len(unique)-1]) {
			unique = append(unique, cut)
		}
	}
	return unique
}

func tariffAt(at time.Time, segments []marginalTariff) *marginalTariff {
	var selected *marginalTariff
	for i := range segments {
		t := &segments[i]
		if !at.Before(t.From) && (t.To == nil || at.Before(*t.To)) {
			if selected != nil {
				return nil
			}
			selected = t
		}
	}
	return selected
}

func priceIntervalRange(item interval, from, to time.Time, schedule marginalTariff) *big.Rat {
	start, end := maxTime(item.Start, from), minTime(item.End, to)
	amount := new(big.Rat)
	if !end.After(start) {
		return amount
	}
	cuts := tariffCuts(start, end, schedule.Segments)
	for i := 0; i+1 < len(cuts); i++ {
		t := tariffAt(cuts[i], schedule.Segments)
		if t == nil {
			return nil
		}
		share := new(big.Rat).SetFrac(big.NewInt(cuts[i+1].Sub(cuts[i]).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
		wh := new(big.Rat).Mul(item.Wh, share)
		amount.Add(amount, new(big.Rat).Mul(new(big.Rat).Quo(wh, big.NewRat(1000, 1)), t.Rate))
	}
	return amount
}

func priceEffectiveBuckets(buckets []bucket, items []interval, schedule marginalTariff) *money {
	var total, displayed big.Rat
	priced := false
	for i := range buckets {
		if buckets[i].energyKWh == nil {
			continue
		}
		for _, item := range items {
			part := priceIntervalRange(item, buckets[i].Start, buckets[i].End, schedule)
			if part == nil {
				return nil
			}
			total.Add(&total, part)
		}
		cumulative := mustDecimal(total.FloatString(6))
		shown := new(big.Rat).Sub(cumulative, &displayed)
		cost := money{Amount: shown.FloatString(6), Currency: schedule.Currency}
		buckets[i].Allocation.Cost = cost
		buckets[i].Allocation.UnclassifiedCost = cost
		buckets[i].Allocation.EvidenceBasis = "mixed"
		displayed.Set(cumulative)
		priced = true
	}
	if !priced {
		return nil
	}
	return &money{Amount: total.FloatString(6), Currency: schedule.Currency}
}

func priceAttributedDevices(buckets []bucket, rows [][]deviceAllocation, periodRows []deviceAllocation, items []interval, schedule marginalTariff) {
	period := make(map[string]*big.Rat)
	displayed := make(map[string]*big.Rat)
	for i := range buckets {
		for j := range rows[i] {
			id := rows[i][j].ApplianceID
			amount := new(big.Rat)
			for _, item := range items {
				if item.ApplianceID != id || item.ParentSensorID != nil || item.SensorKind != "plug" && item.SensorKind != "strip_outlet" {
					continue
				}
				part := priceIntervalRange(item, buckets[i].Start, buckets[i].End, schedule)
				if part != nil {
					amount.Add(amount, part)
				}
			}
			if period[id] == nil {
				period[id], displayed[id] = new(big.Rat), new(big.Rat)
			}
			period[id].Add(period[id], amount)
			cumulative := mustDecimal(period[id].FloatString(6))
			cost := money{Amount: new(big.Rat).Sub(cumulative, displayed[id]).FloatString(6), Currency: schedule.Currency}
			rows[i][j].Allocation.Cost = cost
			rows[i][j].Allocation.UnclassifiedCost = cost
			rows[i][j].Allocation.EvidenceBasis = "mixed"
			displayed[id].Set(cumulative)
		}
	}
	for i := range periodRows {
		id := periodRows[i].ApplianceID
		if period[id] == nil {
			continue
		}
		cost := money{Amount: period[id].FloatString(6), Currency: schedule.Currency}
		periodRows[i].Allocation.Cost = cost
		periodRows[i].Allocation.UnclassifiedCost = cost
		periodRows[i].Allocation.EvidenceBasis = "mixed"
	}
}
