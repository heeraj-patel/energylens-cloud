package householdenergy

import (
	"context"
	"energylens.local/cloud/internal/energyrate"
	tariffpkg "energylens.local/cloud/internal/tariff"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// NationalRate returns the US average residential rate in $/kWh.
type NationalRate func(context.Context) (float64, error)

// schedule prices a home without a tariff at the national rate, or nil when
// no rate is available.
func (n NationalRate) schedule(ctx context.Context) *marginalTariff {
	if n == nil {
		return nil
	}
	value, err := n(ctx)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	rate := mustDecimal(strconv.FormatFloat(value, 'f', 9, 64))
	return &marginalTariff{Currency: "USD", Rate: rate, Segments: []marginalTariff{{Currency: "USD", Rate: rate}}, Basis: "us_average"}
}

// scheduleFor is schedule limited to a nonempty set of available electricity
// intervals, the only ones that can be priced.
func (n NationalRate) scheduleFor(ctx context.Context, items []interval) *marginalTariff {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if item.Unavailable || item.Fuel == nil || *item.Fuel != "electricity" || item.Wh == nil {
			return nil
		}
	}
	return n.schedule(ctx)
}

// nationalBasis is "us_average" for a national-rate schedule, else empty.
func (t *marginalTariff) nationalBasis() string {
	if t == nil {
		return ""
	}
	return t.Basis
}

// markNationalRate tags priced device rows with the national rate basis.
func markNationalRate(t *marginalTariff, rows [][]deviceAllocation, periodRows []deviceAllocation) {
	if t.nationalBasis() == "" {
		return
	}
	mark := func(a *deviceAllocation) {
		if _, ok := a.Allocation.Cost.(money); ok {
			a.Allocation.RateBasis = t.Basis
		}
	}
	for i := range rows {
		for j := range rows[i] {
			mark(&rows[i][j])
		}
	}
	for i := range periodRows {
		mark(&periodRows[i])
	}
}

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
	for _, item := range items {
		if item.Unavailable || item.Fuel == nil || *item.Fuel != "electricity" || item.Wh == nil {
			return latestID, latestRevision, nil, nil
		}
		first, last = minTime(first, item.Start), maxTime(last, item.End)
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
		// A retained hour has only its aggregate energy. A tariff transition
		// within that hour cannot be priced from the retained precision.
		if item.Summarized && len(cuts) > 2 {
			return nil
		}
		for i := 0; i+1 < len(cuts); i++ {
			selected := tariffAt(cuts[i], segments)
			if selected == nil || selected.SourceAsOf.After(now) || !selected.SourceAsOf.After(now.Add(-tariffpkg.ElectricityMaxAge)) {
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
	if item.Summarized && len(cuts) > 2 {
		return nil
	}
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
		selected := buckets[i].selected
		if selected == nil {
			selected = items
		}
		for _, item := range selected {
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
	unpriced := make(map[string]bool)
	for i := range buckets {
		for j := range rows[i] {
			id := rows[i][j].ApplianceID
			amount := new(big.Rat)
			valid := true
			selected := buckets[i].deviceItems
			if selected == nil {
				selected = items
			}
			for _, item := range selected {
				if item.ApplianceID != id || !terminalDevice(item) {
					continue
				}
				part := priceIntervalRange(item, buckets[i].Start, buckets[i].End, schedule)
				if part == nil {
					valid = false
					break
				}
				amount.Add(amount, part)
			}
			if !valid {
				unpriced[id] = true
				continue
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
		if period[id] == nil || unpriced[id] {
			continue
		}
		cost := money{Amount: period[id].FloatString(6), Currency: schedule.Currency}
		periodRows[i].Allocation.Cost = cost
		periodRows[i].Allocation.UnclassifiedCost = cost
		periodRows[i].Allocation.EvidenceBasis = "mixed"
	}
}

// GasRateBasisUserTariff is the gas_rate_basis of the home's own gas tariff.
// The other bases are the energyrate.GasBasis* values.
const GasRateBasisUserTariff = "user_tariff"

// GasRateResolver is the fallback gas rate for a US ZIP ("" means national).
type GasRateResolver func(ctx context.Context, zip string) (energyrate.GasResolved, error)

// GasRate is a home's effective gas price in USD per therm. Basis is the
// gas_rate_basis for L13 to serialise; no rate means a null basis.
type GasRate struct {
	USDPerTherm *big.Rat
	Basis       string
}

var usZIPRe = regexp.MustCompile(`^([0-9]{5})(?:-?[0-9]{4})?$`)

// readEffectiveGasRate returns the home's current user natural_gas tariff, else
// the resolver rate for a US home, else nil. A resolver error means no rate.
func readEffectiveGasRate(ctx context.Context, tx pgx.Tx, home string, now time.Time, resolve GasRateResolver) (*GasRate, error) {
	var rate string
	err := tx.QueryRow(ctx, `SELECT rate::text FROM tariff_revisions
 WHERE home_id=$1 AND fuel='natural_gas' AND rate_unit='therm' AND currency='USD'
 AND rate_kind='user_tariff' AND NOT is_fallback AND source_as_of<=$2
 AND effective_from<=$2 AND (effective_to IS NULL OR effective_to>$2)
 ORDER BY effective_from DESC,revision DESC,id DESC LIMIT 1`, home, now).Scan(&rate)
	if err == nil {
		value, ok := new(big.Rat).SetString(rate)
		if !ok {
			return nil, fmt.Errorf("invalid persisted tariff rate")
		}
		return &GasRate{USDPerTherm: value, Basis: GasRateBasisUserTariff}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if resolve == nil {
		return nil, nil
	}
	var country, postal string
	err = tx.QueryRow(ctx, `SELECT country_code,postal_code FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&country, &postal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if country != "US" {
		return nil, nil
	}
	zip := ""
	if m := usZIPRe.FindStringSubmatch(strings.TrimSpace(postal)); m != nil {
		zip = m[1]
	}
	resolved, err := resolve(ctx, zip)
	if err != nil || resolved.USDPerTherm <= 0 || math.IsNaN(resolved.USDPerTherm) || math.IsInf(resolved.USDPerTherm, 0) {
		return nil, nil
	}
	return &GasRate{USDPerTherm: mustDecimal(strconv.FormatFloat(resolved.USDPerTherm, 'f', 9, 64)), Basis: resolved.Basis}, nil
}
