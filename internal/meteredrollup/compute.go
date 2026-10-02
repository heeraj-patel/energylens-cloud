package meteredrollup

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type intervalRow struct {
	ID         string
	Revision   int64
	Start, End time.Time
	Wh         float64
	Covered    int64
}
type gapRow struct {
	ID          string
	Revision    int64
	First, Last time.Time
	Dropped     int64
}
type sourceRef struct {
	EventID  string `json:"event_id"`
	Revision int64  `json:"revision"`
}

func bucketEnd(x dirtyRow) time.Time {
	if x.Kind == "energy_day" || x.Kind == "gap_day" || x.Kind == "power_day" {
		return nextDay(x.Start)
	}
	return x.Start.Add(time.Hour)
}
func recomputePower(ctx context.Context, tx pgx.Tx, x dirtyRow) error {
	finish := bucketEnd(x)
	rows, err := tx.Query(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.event_id) e.event_id,e.revision FROM ingest_events e
 WHERE e.home_id=$1 AND e.hub_id=$2 AND e.ownership_epoch=$3 AND e.sensor_id=$4 AND e.event_kind='power_point'
 ORDER BY e.event_id,e.revision DESC)
 SELECT l.event_id,l.revision,o.watts FROM latest l
 JOIN energy_observations o ON o.hub_id=$2 AND o.ownership_epoch=$3 AND o.event_id=l.event_id AND o.revision=l.revision
 WHERE o.observed_at >= $5 AND o.observed_at < $6 ORDER BY l.event_id`, x.Home, x.Hub, x.Epoch, x.Sensor, x.Start, finish)
	if err != nil {
		return err
	}
	refs := []sourceRef{}
	var count int
	var min, max, mean float64
	for rows.Next() {
		var id string
		var revision int64
		var watts float64
		if err = rows.Scan(&id, &revision, &watts); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, sourceRef{id, revision})
		if count == 0 || watts < min {
			min = watts
		}
		if count == 0 || watts > max {
			max = watts
		}
		count++
		mean += (watts - mean) / float64(count)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	body, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	var low, high, average any
	if count > 0 {
		low, high, average = min, max, mean
	}
	table := "power_hourly"
	if x.Kind == "power_day" {
		table = "power_daily"
	}
	sql := fmt.Sprintf(`INSERT INTO %s(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,sample_count,min_watts,max_watts,mean_watts,source_refs) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(home_id,hub_id,ownership_epoch,sensor_id,bucket_start) DO UPDATE SET sample_count=excluded.sample_count,min_watts=excluded.min_watts,max_watts=excluded.max_watts,mean_watts=excluded.mean_watts,source_refs=excluded.source_refs,computed_at=now()`, table)
	_, err = tx.Exec(ctx, sql, x.Home, x.Hub, x.Epoch, x.Sensor, x.Start, count, low, high, average, body)
	return err
}
func ratFloat(v float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', -1, 64))
	if !ok {
		panic("validated finite numeric")
	}
	return r
}
func rounded(r *big.Rat) *big.Rat {
	v, ok := new(big.Rat).SetString(r.FloatString(12))
	if !ok {
		panic("decimal rounding")
	}
	return v
}
func allocated(source *big.Rat, start, end, bucketStart, bucketFinish time.Time, day bool) *big.Rat {
	low := start
	if bucketStart.After(low) {
		low = bucketStart
	}
	high := end
	if bucketFinish.Before(high) {
		high = bucketFinish
	}
	if !high.After(low) {
		return new(big.Rat)
	}
	first := floorUTC(start, time.Hour)
	last := floorUTC(end.Add(-time.Nanosecond), time.Hour)
	step := time.Hour
	if day {
		first = dayUTC(start)
		last = dayUTC(end.Add(-time.Nanosecond))
		step = 24 * time.Hour
	}
	if first.Equal(last) {
		return new(big.Rat).Set(source)
	}
	duration := end.Sub(start).Nanoseconds()
	raw := func(b time.Time) *big.Rat {
		finish := b.Add(step)
		lo := start
		if b.After(lo) {
			lo = b
		}
		hi := end
		if finish.Before(hi) {
			hi = finish
		}
		if !hi.After(lo) {
			return new(big.Rat)
		}
		ratio := new(big.Rat).SetFrac(big.NewInt(hi.Sub(lo).Nanoseconds()), big.NewInt(duration))
		return new(big.Rat).Mul(source, ratio)
	}
	if !bucketStart.Equal(last) {
		return rounded(raw(bucketStart))
	}
	prior := new(big.Rat)
	for b := first; b.Before(last); b = b.Add(step) {
		prior.Add(prior, rounded(raw(b)))
	}
	return new(big.Rat).Sub(source, prior)
}
func recomputeEnergy(ctx context.Context, tx pgx.Tx, x dirtyRow) error {
	if x.Attachment == nil {
		return fmt.Errorf("energy bucket missing attachment")
	}
	finish := bucketEnd(x)
	rows, err := tx.Query(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.event_id) e.event_id,e.revision FROM ingest_events e
 WHERE e.home_id=$1 AND e.hub_id=$2 AND e.ownership_epoch=$3 AND e.sensor_id=$4 AND e.event_kind='energy_interval'
 ORDER BY e.event_id,e.revision DESC)
 SELECT l.event_id,l.revision,o.start_at,o.end_at,o.energy_wh,o.covered_seconds
 FROM latest l JOIN energy_observations o ON o.hub_id=$2 AND o.ownership_epoch=$3 AND o.event_id=l.event_id AND o.revision=l.revision
 WHERE o.attachment_revision_id=$5 AND o.start_at<$7 AND o.end_at>$6
 ORDER BY o.start_at,o.end_at,l.event_id`, x.Home, x.Hub, x.Epoch, x.Sensor, *x.Attachment, x.Start, finish)
	if err != nil {
		return err
	}
	intervals := []intervalRow{}
	for rows.Next() {
		var v intervalRow
		if err = rows.Scan(&v.ID, &v.Revision, &v.Start, &v.End, &v.Wh, &v.Covered); err != nil {
			rows.Close()
			return err
		}
		intervals = append(intervals, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	refs := make([]sourceRef, 0, len(intervals))
	for _, v := range intervals {
		refs = append(refs, sourceRef{v.ID, v.Revision})
	}
	status, method := "no_intervals", "none"
	var wh, covered any
	if len(intervals) > 0 {
		conflict := false
		previousEnd := intervals[0].End
		for _, v := range intervals[1:] {
			if v.Start.Before(previousEnd) {
				conflict = true
			}
			if v.End.After(previousEnd) {
				previousEnd = v.End
			}
		}
		if conflict {
			status = "overlap_conflict"
		} else {
			status, method = "complete", "exact"
			sumWh, sumCoverage := new(big.Rat), new(big.Rat)
			day := x.Kind == "energy_day"
			for _, v := range intervals {
				if v.Start.Before(x.Start) || v.End.After(finish) {
					method = "duration_proportional"
				}
				sumWh.Add(sumWh, allocated(ratFloat(v.Wh), v.Start, v.End, x.Start, finish, day))
				sumCoverage.Add(sumCoverage, allocated(new(big.Rat).SetInt64(v.Covered), v.Start, v.End, x.Start, finish, day))
			}
			wh, covered = sumWh.FloatString(12), sumCoverage.FloatString(12)
		}
	}
	body, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	table := "energy_hourly"
	if x.Kind == "energy_day" {
		table = "energy_daily"
	}
	sql := fmt.Sprintf(`INSERT INTO %s(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start,energy_wh,covered_seconds,interval_count,status,allocation_method,source_refs) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(home_id,hub_id,ownership_epoch,sensor_id,attachment_id,bucket_start) DO UPDATE SET energy_wh=excluded.energy_wh,covered_seconds=excluded.covered_seconds,interval_count=excluded.interval_count,status=excluded.status,allocation_method=excluded.allocation_method,source_refs=excluded.source_refs,computed_at=now()`, table)
	_, err = tx.Exec(ctx, sql, x.Home, x.Hub, x.Epoch, x.Sensor, *x.Attachment, x.Start, wh, covered, len(intervals), status, method, body)
	return err
}
func recomputeGap(ctx context.Context, tx pgx.Tx, x dirtyRow) error {
	finish := bucketEnd(x)
	rows, err := tx.Query(ctx, `WITH latest AS (
 SELECT DISTINCT ON (e.event_id) e.event_id,e.revision FROM ingest_events e
 WHERE e.home_id=$1 AND e.hub_id=$2 AND e.ownership_epoch=$3 AND e.sensor_id=$4 AND e.event_kind='coverage_gap'
 ORDER BY e.event_id,e.revision DESC)
 SELECT l.event_id,l.revision,g.first_at,g.last_at,g.dropped_count
 FROM latest l JOIN coverage_gaps g ON g.hub_id=$2 AND g.ownership_epoch=$3 AND g.event_id=l.event_id AND g.revision=l.revision
 WHERE g.first_at<$6 AND g.last_at>=$5 ORDER BY g.first_at,l.event_id`, x.Home, x.Hub, x.Epoch, x.Sensor, x.Start, finish)
	if err != nil {
		return err
	}
	gaps := []gapRow{}
	for rows.Next() {
		var v gapRow
		if err = rows.Scan(&v.ID, &v.Revision, &v.First, &v.Last, &v.Dropped); err != nil {
			rows.Close()
			return err
		}
		gaps = append(gaps, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].ID < gaps[j].ID })
	refs := make([]sourceRef, 0, len(gaps))
	var dropped int64
	for _, v := range gaps {
		refs = append(refs, sourceRef{v.ID, v.Revision})
		if !v.First.Before(x.Start) && v.First.Before(finish) {
			dropped += v.Dropped
		}
	}
	body, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	table := "coverage_hourly"
	if x.Kind == "gap_day" {
		table = "coverage_daily"
	}
	sql := fmt.Sprintf(`INSERT INTO %s(home_id,hub_id,ownership_epoch,sensor_id,bucket_start,gap_overlap_count,dropped_count_started,source_refs) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(home_id,hub_id,ownership_epoch,sensor_id,bucket_start) DO UPDATE SET gap_overlap_count=excluded.gap_overlap_count,dropped_count_started=excluded.dropped_count_started,source_refs=excluded.source_refs,computed_at=now()`, table)
	_, err = tx.Exec(ctx, sql, x.Home, x.Hub, x.Epoch, x.Sensor, x.Start, len(gaps), dropped, body)
	return err
}
