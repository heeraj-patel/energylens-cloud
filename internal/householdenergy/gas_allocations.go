package householdenergy

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"energylens.local/cloud/internal/gasestimate"
	"github.com/jackc/pgx/v5"
)

// gasAppliance is a natural-gas or propane appliance and its yearly estimate.
// Metered appliances keep their measured electric row; gas is added to it.
type gasAppliance struct {
	id, fuel string
	metered  bool
	estimate gasestimate.Estimate
}

// gasContext holds the home's gas appliances and its gas rate (nil: no rate).
type gasContext struct {
	appliances []gasAppliance
	rate       *GasRate
}

// loadGasContext returns nil for a home with no estimable gas appliance, so
// electric homes never resolve a gas rate. Fuel oil has no API shape yet.
func loadGasContext(ctx context.Context, tx pgx.Tx, home string, now time.Time, resolve GasRateResolver) (*gasContext, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text,a.category,a.identity->>'fuel',a.catalog_product_id::text,EXISTS(SELECT 1 FROM attachments t JOIN sensor_endpoints s ON s.id=t.sensor_id WHERE t.appliance_id=a.id AND t.ended_at IS NULL AND s.archived_at IS NULL AND s.capabilities @> ARRAY['power','energy']::text[]) FROM appliances a WHERE a.home_id=$1 AND a.archived_at IS NULL AND a.identity->>'fuel' IN ('natural_gas','propane') ORDER BY a.id`, home)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, category, fuel string
		product            *string
		metered            bool
	}
	var found []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.category, &r.fuel, &r.product, &r.metered); err != nil {
			break
		}
		found = append(found, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	gc := &gasContext{}
	for _, r := range found {
		e, ok, err := gasestimate.ForAppliance(ctx, tx, r.category, r.fuel, r.product)
		if err != nil {
			return nil, err
		}
		if ok && (e.ElectricKWh != nil || e.GasTherms != nil) {
			gc.appliances = append(gc.appliances, gasAppliance{id: r.id, fuel: r.fuel, metered: r.metered, estimate: e})
		}
	}
	if len(gc.appliances) == 0 {
		return nil, nil
	}
	if gc.rate, err = readEffectiveGasRate(ctx, tx, home, now, resolve); err != nil {
		return nil, err
	}
	// A rate without a basis would be an unflagged price; treat it as no rate.
	if gc.rate != nil && (gc.rate.Basis == "" || gc.rate.USDPerTherm == nil || gc.rate.USDPerTherm.Sign() <= 0) {
		gc.rate = nil
	}
	return gc, nil
}

// gasFigures are one span's amounts; nil means unknown.
type gasFigures struct {
	electric, standby, therms, kwhEq *big.Rat
	cost, standbyCost                *big.Rat
	unpriced                         bool
}

func (f gasFigures) empty() bool { return f.electric == nil && f.therms == nil }

func (f *gasFigures) add(o gasFigures) {
	addRat(&f.electric, o.electric)
	addRat(&f.standby, o.standby)
	addRat(&f.therms, o.therms)
	addRat(&f.kwhEq, o.kwhEq)
	addRat(&f.cost, o.cost)
	addRat(&f.standbyCost, o.standbyCost)
	f.unpriced = f.unpriced || o.unpriced
}

func addRat(sum **big.Rat, v *big.Rat) {
	if v == nil {
		return
	}
	if *sum == nil {
		*sum = new(big.Rat)
	}
	(*sum).Add(*sum, v)
}

// annualShare is the part of a yearly figure falling in [start,end), by local day.
func annualShare(annual float64, start, end time.Time) float64 {
	var out float64
	for at := start; at.Before(end); {
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		next := day.AddDate(0, 0, 1)
		stop := end
		if next.Before(stop) {
			stop = next
		}
		days := 365.0
		if day.Year()%4 == 0 && (day.Year()%100 != 0 || day.Year()%400 == 0) {
			days = 366
		}
		out += annual / days * stop.Sub(at).Seconds() / next.Sub(day).Seconds()
		at = stop
	}
	return out
}

func spanRat(annual *float64, start, end time.Time) *big.Rat {
	if annual == nil || math.IsNaN(*annual) || math.IsInf(*annual, 0) || *annual < 0 {
		return nil
	}
	return mustDecimal(fmt.Sprintf("%.9f", annualShare(*annual, start, end)))
}

// gasOnly is the span's gas amounts; the electric part is left to measurement.
func (g gasAppliance) gasOnly(start, end time.Time) gasFigures {
	return gasFigures{therms: spanRat(g.estimate.GasTherms, start, end), kwhEq: spanRat(g.estimate.GasKWhEq, start, end)}
}

// figures adds the estimated electric part, priced by tariff when there is one.
func (g gasAppliance) figures(start, end time.Time, tariff *marginalTariff) gasFigures {
	f := g.gasOnly(start, end)
	f.electric = spanRat(g.estimate.ElectricKWh, start, end)
	f.standby = spanRat(g.estimate.StandbyKWh, start, end)
	if tariff == nil {
		return f
	}
	price := func(kwh *big.Rat) *big.Rat {
		if kwh == nil {
			return nil
		}
		piece := interval{Start: start, End: end, Wh: new(big.Rat).Mul(kwh, big.NewRat(1000, 1))}
		return priceIntervalRange(piece, start, end, *tariff)
	}
	f.cost, f.standbyCost = price(f.electric), price(f.standby)
	f.unpriced = f.electric != nil && f.cost == nil || f.standby != nil && f.standbyCost == nil
	return f
}

// wasteBasis is rated_standby for a per-model figure, standby for a typical.
func (g gasAppliance) wasteBasis() string {
	if g.estimate.StandbyBasis == gasestimate.BasisTypical {
		return "standby"
	}
	return "rated_standby"
}

// estimatedAllocation builds a gas appliance's row from its estimate. The
// electric part is an estimate beside, never inside, the metered totals.
func (g gasAppliance) estimatedAllocation(f gasFigures, tariff *marginalTariff, rateBasis string, rate *GasRate) allocation {
	a := allocation{Availability: "partial", EvidenceBasis: "estimated"}
	if f.electric != nil {
		a.Energy = &quantity{Value: f.electric.FloatString(12), Unit: "kWh"}
		a.UnclassifiedEnergy = a.Energy
		if tariff != nil && !f.unpriced {
			a.Cost = money{Amount: f.cost.FloatString(6), Currency: tariff.Currency}
			a.UnclassifiedCost = a.Cost
			a.RateBasis = rateBasis
		}
		if f.standby != nil {
			standby := f.standby
			if standby.Cmp(f.electric) > 0 {
				standby = f.electric
			}
			a.WasteBasis = g.wasteBasis()
			a.WasteEnergy = &quantity{Value: standby.FloatString(12), Unit: "kWh"}
			a.UsefulEnergy = &quantity{Value: nonNegative(new(big.Rat).Sub(f.electric, standby)).FloatString(12), Unit: "kWh"}
			a.UnclassifiedEnergy = nil
			if cost, ok := a.Cost.(money); ok && f.standbyCost != nil {
				wasteCost := f.standbyCost
				if wasteCost.Cmp(f.cost) > 0 {
					wasteCost = f.cost
				}
				a.WasteCost = money{Amount: wasteCost.FloatString(6), Currency: cost.Currency}
				a.StandbyCost = a.WasteCost
				a.UsefulCost = money{Amount: nonNegative(new(big.Rat).Sub(f.cost, wasteCost)).FloatString(6), Currency: cost.Currency}
				a.UnclassifiedCost = nil
			}
		}
	}
	applyGas(&a, g, f, rate)
	return a
}

// applyGas sets the fuel and gas fields on a row whose electric part is
// already set. gas_cost needs both therms and a rate; total_cost needs gas_cost
// and a known electric cost, so the UI falls back to cost otherwise.
func applyGas(a *allocation, g gasAppliance, f gasFigures, rate *GasRate) {
	a.Fuel = g.fuel
	if f.therms == nil || f.kwhEq == nil {
		return
	}
	a.GasEnergy = &fuelQuantity{Value: f.therms.FloatString(12), Unit: "therm"}
	a.GasEnergyKWhEq = &quantity{Value: f.kwhEq.FloatString(12), Unit: "kWh"}
	a.GasBasis = g.estimate.GasBasis
	total := new(big.Rat).Set(f.kwhEq)
	if a.Energy != nil {
		total.Add(total, mustDecimal(a.Energy.Value))
	}
	a.TotalEnergyKWhEq = &quantity{Value: total.FloatString(12), Unit: "kWh"}
	if rate == nil {
		return
	}
	gasCost := new(big.Rat).Mul(f.therms, rate.USDPerTherm)
	a.GasCost = money{Amount: gasCost.FloatString(6), Currency: "USD"}
	a.GasRateBasis = rate.Basis
	switch cost := a.Cost.(type) {
	case money:
		if cost.Currency == "USD" {
			a.TotalCost = money{Amount: new(big.Rat).Add(mustDecimal(cost.Amount), gasCost).FloatString(6), Currency: "USD"}
		}
	default:
		if a.Energy == nil {
			a.TotalCost = a.GasCost
		}
	}
}

// gasDeviceRows are the estimated gas rows, per bucket and for the period:
// whole rows for non-metered appliances, and for metered ones only the buckets
// with no measured row (standby fallback), merged into the measured period row
// in existing, which is updated in place.
func gasDeviceRows(ctx context.Context, tx pgx.Tx, gc *gasContext, home string, buckets []bucket, now time.Time, national NationalRate, existing []deviceAllocation) ([][]deviceAllocation, []deviceAllocation, error) {
	byBucket := make([][]deviceAllocation, len(buckets))
	period := []deviceAllocation{}
	if gc == nil || len(buckets) == 0 || len(gc.appliances) == 0 {
		return byBucket, period, nil
	}
	tariff, rateBasis, err := estimateElectricSchedule(ctx, tx, home, now, buckets, national)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range gc.appliances {
		var total gasFigures
		var fallback fallbackSum
		for bi, b := range buckets {
			if g.metered && hasDeviceRow(buckets[bi].DeviceAllocations, g.id) {
				continue
			}
			f := g.figures(b.Start, b.End, tariff)
			if f.empty() {
				continue
			}
			total.add(f)
			a := g.estimatedAllocation(f, tariff, rateBasis, gc.rate)
			fallback.add(a)
			byBucket[bi] = append(byBucket[bi], deviceAllocation{ApplianceID: g.id, Allocation: a})
		}
		if total.empty() {
			continue
		}
		if !g.metered {
			period = append(period, deviceAllocation{ApplianceID: g.id, Allocation: g.estimatedAllocation(total, tariff, rateBasis, gc.rate)})
			continue
		}
		var all gasFigures
		for _, b := range buckets {
			all.add(g.gasOnly(b.Start, b.End))
		}
		merged := false
		for j := range existing {
			if existing[j].ApplianceID == g.id {
				fallback.mergeInto(&existing[j].Allocation, true, rateBasis, g.wasteBasis())
				applyGas(&existing[j].Allocation, g, all, gc.rate)
				merged = true
			}
		}
		if !merged {
			a := allocation{Availability: "partial", EvidenceBasis: "estimated"}
			fallback.mergeInto(&a, false, rateBasis, g.wasteBasis())
			applyGas(&a, g, all, gc.rate)
			period = append(period, deviceAllocation{ApplianceID: g.id, Allocation: a})
		}
	}
	return byBucket, period, nil
}

func hasDeviceRow(rows []deviceAllocation, id string) bool {
	for _, r := range rows {
		if r.ApplianceID == id {
			return true
		}
	}
	return false
}

// fallbackSum adds up the estimated bucket rows of one metered gas appliance,
// so its period row equals the sum of the bucket rows it shows.
type fallbackSum struct {
	energy, useful, waste, unclass           *big.Rat
	cost, usefulCost, wasteCost, unclassCost *big.Rat
	standbyCost                              *big.Rat
	currency                                 string
	unpriced                                 bool
}

func ratOf(q any) *big.Rat {
	switch v := q.(type) {
	case *quantity:
		if v != nil {
			return mustDecimal(v.Value)
		}
	case money:
		return mustDecimal(v.Amount)
	}
	return nil
}

func (s *fallbackSum) add(a allocation) {
	if a.Energy == nil {
		return
	}
	addRat(&s.energy, ratOf(a.Energy))
	addRat(&s.useful, ratOf(a.UsefulEnergy))
	addRat(&s.waste, ratOf(a.WasteEnergy))
	addRat(&s.unclass, ratOf(a.UnclassifiedEnergy))
	m, ok := a.Cost.(money)
	if !ok {
		s.unpriced = true
		return
	}
	s.currency = m.Currency
	addRat(&s.cost, ratOf(a.Cost))
	addRat(&s.usefulCost, ratOf(a.UsefulCost))
	addRat(&s.wasteCost, ratOf(a.WasteCost))
	addRat(&s.unclassCost, ratOf(a.UnclassifiedCost))
	addRat(&s.standbyCost, ratOf(a.StandbyCost))
}

// mergeInto adds the fallback to a period row; fields the row lacks count as
// zero, and cost fields are dropped when either part has no electric price.
func (s fallbackSum) mergeInto(p *allocation, measured bool, rateBasis, wasteBasis string) {
	if s.energy == nil {
		return
	}
	sum := func(old any, add *big.Rat) *big.Rat {
		var out *big.Rat
		addRat(&out, ratOf(old))
		addRat(&out, add)
		return out
	}
	qty := func(r *big.Rat) *quantity {
		if r == nil {
			return nil
		}
		return &quantity{Value: r.FloatString(12), Unit: "kWh"}
	}
	p.Energy = qty(sum(p.Energy, s.energy))
	if r := sum(p.UnclassifiedEnergy, s.unclass); r != nil {
		p.UnclassifiedEnergy = qty(r)
	}
	if r := sum(p.UsefulEnergy, s.useful); r != nil {
		p.UsefulEnergy = qty(r)
	}
	if r := sum(p.WasteEnergy, s.waste); r != nil {
		p.WasteEnergy = qty(r)
		if p.WasteBasis == "" {
			p.WasteBasis = wasteBasis
		}
	}
	if p.RateBasis == "" {
		p.RateBasis = rateBasis
	}
	p.Availability = "partial"
	p.EvidenceBasis = "estimated"
	if measured {
		p.EvidenceBasis = "mixed"
	}
	cur, isMoney := p.Cost.(money)
	if s.unpriced || s.currency == "" || measured && (!isMoney || cur.Currency != s.currency) {
		p.Cost, p.UsefulCost, p.WasteCost, p.UnclassifiedCost, p.StandbyCost = nil, nil, nil, nil, nil
		return
	}
	set := func(old any, add *big.Rat) any {
		if r := sum(old, add); r != nil {
			return money{Amount: r.FloatString(6), Currency: s.currency}
		}
		return old
	}
	p.Cost = set(p.Cost, s.cost)
	p.UsefulCost = set(p.UsefulCost, s.usefulCost)
	p.WasteCost = set(p.WasteCost, s.wasteCost)
	p.UnclassifiedCost = set(p.UnclassifiedCost, s.unclassCost)
	p.StandbyCost = set(p.StandbyCost, s.standbyCost)
}

// decorate adds gas fields to the measured rows of metered gas appliances.
// Their electric figures stay measured; gas is never metered, so the period row
// carries gas for every requested bucket. gasDeviceRows fills unmeasured buckets.
func (gc *gasContext) decorate(buckets []bucket, rows [][]deviceAllocation, periodRows []deviceAllocation) {
	if gc == nil {
		return
	}
	for _, g := range gc.appliances {
		if !g.metered {
			continue
		}
		var total gasFigures
		for bi := range buckets {
			total.add(g.gasOnly(buckets[bi].Start, buckets[bi].End))
		}
		for bi := range rows {
			for j := range rows[bi] {
				if rows[bi][j].ApplianceID != g.id {
					continue
				}
				applyGas(&rows[bi][j].Allocation, g, g.gasOnly(buckets[bi].Start, buckets[bi].End), gc.rate)
			}
		}
		for j := range periodRows {
			if periodRows[j].ApplianceID == g.id {
				applyGas(&periodRows[j].Allocation, g, total, gc.rate)
			}
		}
	}
}

// gasCostTotal is the home's separate gas line (T1): the sum of the gas
// appliance rows' gas_cost, or nil when none is priced.
func gasCostTotal(rows []deviceAllocation) *money {
	var sum *big.Rat
	for _, d := range rows {
		if m, ok := d.Allocation.GasCost.(money); ok {
			addRat(&sum, mustDecimal(m.Amount))
		}
	}
	if sum == nil {
		return nil
	}
	return &money{Amount: sum.FloatString(6), Currency: "USD"}
}
