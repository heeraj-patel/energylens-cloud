package householdenergy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"energylens.local/cloud/internal/democost"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DemoSnapshotRevision = "mobile_demo_cost_v1"

// DemoCostSnapshot is an explicitly imported cost-only synthetic snapshot.
// Values are integer microcurrency units; no energy or tariff is inferred.
type DemoCostSnapshot = democost.Snapshot
type DemoSnapshotCosts = democost.Costs
type DemoSnapshotDevice = democost.Device
type DemoSnapshotAttribution = democost.Attribution

func DecodeDemoCostSnapshot(r io.Reader) (DemoCostSnapshot, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var snapshot DemoCostSnapshot
	if err := dec.Decode(&snapshot); err != nil {
		return snapshot, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return snapshot, errors.New("snapshot must contain one JSON value")
	}
	if err := snapshot.Validate(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// ImportDemoCostSnapshot replaces only the explicitly scoped home's snapshot.
// Ownership is verified while holding the row lock, in the same transaction.
func ImportDemoCostSnapshot(ctx context.Context, pool *pgxpool.Pool, ownerID, homeID string, snapshot DemoCostSnapshot) error {
	if _, err := uuid.Parse(ownerID); err != nil {
		return errors.New("valid owner UUID required")
	}
	if _, err := uuid.Parse(homeID); err != nil {
		return errors.New("valid home UUID required")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var homeTimezone string
	err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, homeID, ownerID).Scan(&homeTimezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("owner-scoped home not found")
	}
	if err != nil {
		return err
	}
	if snapshot.Timezone != homeTimezone {
		return errors.New("snapshot timezone does not match home timezone")
	}
	if _, err = tx.Exec(ctx, `UPDATE homes SET demo_cost_snapshot=$3::jsonb, version=version+1 WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, homeID, ownerID, string(data)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func sampleCostAlloc(c DemoSnapshotCosts, fraction int64) allocation {
	useful, waste := c.Useful*fraction/1_000_000, c.Waste*fraction/1_000_000
	return allocation{Cost: moneyMicros(useful + waste), UsefulCost: moneyMicros(useful), WasteCost: moneyMicros(waste), Availability: "partial", EvidenceBasis: "synthetic"}
}
func moneyMicros(v int64) money {
	return money{Amount: fmt.Sprintf("%d.%06d", v/1_000_000, v%1_000_000), Currency: "USD"}
}

func projectDemoSnapshot(snapshot DemoCostSnapshot, p period, granularity string, now time.Time) ([]bucket, allocation, []deviceAllocation, *allocation) {
	loc, _ := time.LoadLocation(snapshot.Timezone)
	out := makeBuckets(p, granularity, nil)
	for bi := range out {
		row := &out[bi]
		if granularity == "hour" {
			localStart := row.Start.In(loc)
			day := localStart.Format("2006-01-02")
			if row.Start.After(now) {
				continue
			}
			dayStart := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, loc)
			dayEnd := dayStart.AddDate(0, 0, 1)
			through := row.End
			if through.After(now) {
				through = now
			}
			fromFraction := localDayFraction(row.Start, dayStart, dayEnd)
			throughFraction := localDayFraction(through, dayStart, dayEnd)
			costs := snapshotForDate(snapshot, day)
			rows := make([]deviceAllocation, len(snapshot.Devices))
			for i, d := range snapshot.Devices {
				c := scaleCostsInterval(costs[i], fromFraction, throughFraction)
				rows[i] = syntheticDevice(d, c)
			}
			uc := scaleCostsInterval(costs[len(costs)-1], fromFraction, throughFraction)
			ua := sampleCostAlloc(uc, 1_000_000)
			row.DeviceAllocations = rows
			row.Unattributed = &ua
			row.Allocation = sumCostAllocations(rows, ua)
			continue
		}
		rowStart := row.Start.In(loc)
		rowEnd := row.End.In(loc)
		localNow := now.In(loc)
		today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
		if rowStart.After(today) {
			continue
		}
		deviceCosts := make([]DemoSnapshotCosts, len(snapshot.Devices))
		var unattributedCosts DemoSnapshotCosts
		populatedDays := 0
		for day := rowStart; day.Before(rowEnd) && !day.After(today); day = day.AddDate(0, 0, 1) {
			date := day.Format("2006-01-02")
			costs := snapshotForDate(snapshot, date)
			fraction := int64(1_000_000)
			if day.Equal(today) {
				fraction = localDayFraction(now, day, day.AddDate(0, 0, 1))
			}
			for i, c := range costs[:len(snapshot.Devices)] {
				part := scaleCosts(c, fraction)
				deviceCosts[i].Useful += part.Useful
				deviceCosts[i].Waste += part.Waste
			}
			part := scaleCosts(costs[len(costs)-1], fraction)
			unattributedCosts.Useful += part.Useful
			unattributedCosts.Waste += part.Waste
			populatedDays++
		}
		if populatedDays > 0 {
			row.DeviceAllocations = make([]deviceAllocation, len(snapshot.Devices))
			for i, d := range snapshot.Devices {
				row.DeviceAllocations[i] = syntheticDevice(d, deviceCosts[i])
			}
			ua := sampleCostAlloc(unattributedCosts, 1_000_000)
			row.Unattributed = &ua
			row.Allocation = sumCostAllocations(row.DeviceAllocations, ua)
		}
	}
	// Device totals are independently accumulated over actual projected dates.
	// Rewalk bucket rows to preserve per-device reconciliation for coarse windows.
	byDevice := make([]DemoSnapshotCosts, len(snapshot.Devices))
	var unUseful, unWaste int64
	anyProjected := false
	for _, b := range out {
		if len(b.DeviceAllocations) > 0 {
			anyProjected = true
		}
		for _, d := range b.DeviceAllocations {
			for i, sd := range snapshot.Devices {
				if d.DeviceID == sd.ID {
					byDevice[i].Useful += moneyMicrosUnits(d.Allocation.UsefulCost)
					byDevice[i].Waste += moneyMicrosUnits(d.Allocation.WasteCost)
				}
			}
		}
		if b.Unattributed != nil {
			unUseful += moneyMicrosUnits(b.Unattributed.UsefulCost)
			unWaste += moneyMicrosUnits(b.Unattributed.WasteCost)
		}
	}
	deviceRows := make([]deviceAllocation, 0, len(snapshot.Devices))
	for i, d := range snapshot.Devices {
		if anyProjected {
			deviceRows = append(deviceRows, syntheticDevice(d, byDevice[i]))
		}
	}
	ua := sampleCostAlloc(DemoSnapshotCosts{Useful: unUseful, Waste: unWaste}, 1_000_000)
	total := sumCostAllocations(deviceRows, ua)
	return out, total, deviceRows, &ua
}

func snapshotForDate(s DemoCostSnapshot, date string) []DemoSnapshotCosts {
	out := make([]DemoSnapshotCosts, 0, len(s.Devices)+1)
	ref := date == s.ReferenceLocalDate
	adj := date == s.AdjustmentLocalDate
	for _, d := range s.Devices {
		if ref {
			out = append(out, d.Reference)
		} else {
			out = append(out, d.Default)
		}
	}
	if adj && s.Unattributed.Adjustment != nil {
		out = append(out, *s.Unattributed.Adjustment)
	} else if ref {
		out = append(out, s.Unattributed.Reference)
	} else {
		out = append(out, s.Unattributed.Default)
	}
	return out
}
func scaleCosts(c DemoSnapshotCosts, f int64) DemoSnapshotCosts {
	return DemoSnapshotCosts{Useful: c.Useful * f / 1_000_000, Waste: c.Waste * f / 1_000_000}
}
func localDayFraction(at, start, end time.Time) int64 {
	if !at.After(start) {
		return 0
	}
	if !at.Before(end) {
		return 1_000_000
	}
	return at.Sub(start).Microseconds() * 1_000_000 / end.Sub(start).Microseconds()
}
func scaleCostsInterval(c DemoSnapshotCosts, from, through int64) DemoSnapshotCosts {
	return DemoSnapshotCosts{
		Useful: c.Useful*through/1_000_000 - c.Useful*from/1_000_000,
		Waste:  c.Waste*through/1_000_000 - c.Waste*from/1_000_000,
	}
}
func syntheticDevice(d DemoSnapshotDevice, c DemoSnapshotCosts) deviceAllocation {
	return deviceAllocation{DeviceID: d.ID, Name: d.Name, Allocation: sampleCostAlloc(c, 1_000_000)}
}
func sumCostAllocations(rows []deviceAllocation, extras ...allocation) allocation {
	var u, w int64
	n := 0
	for _, r := range rows {
		u += moneyMicrosUnits(r.Allocation.UsefulCost)
		w += moneyMicrosUnits(r.Allocation.WasteCost)
		n++
	}
	for _, a := range extras {
		u += moneyMicrosUnits(a.UsefulCost)
		w += moneyMicrosUnits(a.WasteCost)
		n++
	}
	if n == 0 {
		return allocation{Availability: "unavailable", EvidenceBasis: "synthetic"}
	}
	return allocation{Cost: moneyMicros(u + w), UsefulCost: moneyMicros(u), WasteCost: moneyMicros(w), Availability: "partial", EvidenceBasis: "synthetic"}
}
func ptr(a allocation) *allocation { return &a }
func moneyMicrosUnits(value any) int64 {
	m, ok := value.(money)
	if !ok {
		return 0
	}
	var whole, fraction int64
	_, _ = fmt.Sscanf(m.Amount, "%d.%d", &whole, &fraction)
	return whole*1_000_000 + fraction
}
