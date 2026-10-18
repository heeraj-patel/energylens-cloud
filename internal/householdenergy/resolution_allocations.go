package householdenergy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

type resolvedEnergy struct {
	Status     string `json:"status"`
	DeviceKind string `json:"device_kind"`
	Values     *struct {
		AnnualKWh      *float64 `json:"annual_kwh"`
		KWhPerDay      *float64 `json:"kwh_per_day"`
		WasteKWhPerDay *float64 `json:"waste_kwh_per_day"`
	} `json:"values"`
}

// resolutionDeviceRows keep estimates beside measured allocations. They never
// enter whole-home measured totals, which may already include these loads.
func resolutionDeviceRows(ctx context.Context, tx pgx.Tx, home string, buckets []bucket, now time.Time) ([][]deviceAllocation, []deviceAllocation, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text,r.record FROM appliances a JOIN datawiz_energy_resolutions r ON r.appliance_ref='cloud:'||a.id::text AND r.record->'appliance_version'=to_jsonb(a.version) WHERE a.home_id=$1 AND a.archived_at IS NULL AND NOT EXISTS(SELECT 1 FROM attachments t JOIN sensor_endpoints s ON s.id=t.sensor_id WHERE t.appliance_id=a.id AND t.ended_at IS NULL AND s.archived_at IS NULL AND s.capabilities @> ARRAY['power','energy']::text[]) ORDER BY a.id`, home)
	if err != nil {
		return nil, nil, err
	}
	type device struct {
		id         string
		resolution resolvedEnergy
	}
	devices := []device{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var record resolvedEnergy
		if err = json.Unmarshal(raw, &record); err != nil {
			break
		}
		if record.Status == "resolved" && record.DeviceKind == "real_non_metering" && record.Values != nil {
			devices = append(devices, device{id, record})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	byBucket := make([][]deviceAllocation, len(buckets))
	period := []deviceAllocation{}
	if len(devices) == 0 || len(buckets) == 0 {
		return byBucket, period, nil
	}
	electricity := "electricity"
	probe := interval{Start: buckets[0].Start, End: buckets[len(buckets)-1].End, Wh: big.NewRat(1, 1), Fuel: &electricity}
	_, _, tariff, err := readEffectiveTariff(ctx, tx, home, now, []interval{probe})
	if err != nil {
		return nil, nil, err
	}
	for _, d := range devices {
		var total, allWaste, totalCost, allWasteCost big.Rat
		hasWaste := d.resolution.Values.WasteKWhPerDay != nil
		priced := tariff != nil
		for bi, b := range buckets {
			energy, waste := resolutionKWhForPeriod(d.resolution, b.Start, b.End)
			if energy <= 0 || math.IsNaN(energy) || math.IsInf(energy, 0) {
				continue
			}
			e := mustDecimal(fmt.Sprintf("%.9f", energy))
			total.Add(&total, e)
			a := fromKWh(e)
			a.EvidenceBasis = "estimated"
			if hasWaste {
				w := mustDecimal(fmt.Sprintf("%.9f", waste))
				allWaste.Add(&allWaste, w)
				a.WasteEnergy = &quantity{Value: w.FloatString(12), Unit: "kWh"}
				remaining := new(big.Rat).Sub(e, w)
				if remaining.Sign() <= 0 {
					a.UnclassifiedEnergy = nil
				} else {
					a.UnclassifiedEnergy = &quantity{Value: remaining.FloatString(12), Unit: "kWh"}
				}
			}
			if tariff != nil {
				piece := interval{Start: b.Start, End: b.End, Wh: new(big.Rat).Mul(e, big.NewRat(1000, 1)), Fuel: &electricity}
				cost := priceIntervalRange(piece, b.Start, b.End, *tariff)
				if cost == nil {
					priced = false
				} else {
					totalCost.Add(&totalCost, cost)
					a.Cost = money{Amount: cost.FloatString(6), Currency: tariff.Currency}
					a.UnclassifiedCost = a.Cost
				}
				if hasWaste && cost != nil {
					piece.Wh = new(big.Rat).Mul(mustDecimal(fmt.Sprintf("%.9f", waste)), big.NewRat(1000, 1))
					wc := priceIntervalRange(piece, b.Start, b.End, *tariff)
					if wc == nil {
						priced = false
					} else {
						allWasteCost.Add(&allWasteCost, wc)
						a.WasteCost = money{Amount: wc.FloatString(6), Currency: tariff.Currency}
						remaining := new(big.Rat).Sub(cost, wc)
						if remaining.Sign() <= 0 {
							a.UnclassifiedCost = nil
						} else {
							a.UnclassifiedCost = money{Amount: remaining.FloatString(6), Currency: tariff.Currency}
						}
					}
				}
			}
			byBucket[bi] = append(byBucket[bi], deviceAllocation{ApplianceID: d.id, Allocation: a})
		}
		if total.Sign() == 0 {
			continue
		}
		a := fromKWh(&total)
		a.EvidenceBasis = "estimated"
		if hasWaste {
			a.WasteEnergy = &quantity{Value: allWaste.FloatString(12), Unit: "kWh"}
			remaining := new(big.Rat).Sub(&total, &allWaste)
			if remaining.Sign() <= 0 {
				a.UnclassifiedEnergy = nil
			} else {
				a.UnclassifiedEnergy = &quantity{Value: remaining.FloatString(12), Unit: "kWh"}
			}
		}
		if priced {
			a.Cost = money{Amount: totalCost.FloatString(6), Currency: tariff.Currency}
			a.UnclassifiedCost = a.Cost
			if hasWaste {
				a.WasteCost = money{Amount: allWasteCost.FloatString(6), Currency: tariff.Currency}
				remaining := new(big.Rat).Sub(&totalCost, &allWasteCost)
				if remaining.Sign() <= 0 {
					a.UnclassifiedCost = nil
				} else {
					a.UnclassifiedCost = money{Amount: remaining.FloatString(6), Currency: tariff.Currency}
				}
			}
		}
		period = append(period, deviceAllocation{ApplianceID: d.id, Allocation: a})
	}
	return byBucket, period, nil
}

func resolutionKWhForPeriod(record resolvedEnergy, start, end time.Time) (float64, float64) {
	var energy, waste float64
	for at := start; at.Before(end); {
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		next := day.AddDate(0, 0, 1)
		stop := end
		if next.Before(stop) {
			stop = next
		}
		fraction := stop.Sub(at).Seconds() / next.Sub(day).Seconds()
		daily := 0.0
		if record.Values.KWhPerDay != nil {
			daily = *record.Values.KWhPerDay
		} else if record.Values.AnnualKWh != nil {
			days := 365.0
			if day.Year()%4 == 0 && (day.Year()%100 != 0 || day.Year()%400 == 0) {
				days = 366
			}
			daily = *record.Values.AnnualKWh / days
		}
		energy += daily * fraction
		if record.Values.WasteKWhPerDay != nil {
			waste += *record.Values.WasteKWhPerDay * fraction
		}
		at = stop
	}
	return energy, waste
}
