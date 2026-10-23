package householdenergy

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// awayBlock is one home-local away window on the listed days.
type awayBlock struct {
	Days       []string `json:"days"`
	StartLocal string   `json:"start_local"`
	EndLocal   string   `json:"end_local"`
}

// awaySchedule is the user's away hours; nil means use defaultAwaySchedule.
type awaySchedule []awayBlock

const (
	awaySourceUser    = "user_schedule"
	awaySourceDefault = "default_schedule"
)

var defaultAwaySchedule = awaySchedule{{
	Days:       []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
	StartLocal: "09:00",
	EndLocal:   "16:00",
}}

var weekdayAbbrev = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadAwaySchedule returns the home's user schedule, or nil when none is set.
func loadAwaySchedule(ctx context.Context, q rowQuerier, home string) (awaySchedule, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT away_schedule FROM home_preferences WHERE home_id=$1`, home).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && raw == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s awaySchedule
	if err = json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return s, nil
}

func clockMinutes(s string) (int, bool) {
	if s == "24:00" {
		return 24 * 60, true
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// awaySpans returns the parts of [from,to) inside the schedule's away blocks,
// merged and with the night window removed so away never double counts night.
func awaySpans(from, to time.Time, loc *time.Location, sched awaySchedule) [][2]time.Time {
	if !to.After(from) {
		return nil
	}
	first := from.In(loc)
	var raw [][2]time.Time
	for n := -1; ; n++ {
		day := time.Date(first.Year(), first.Month(), first.Day()+n, 0, 0, 0, 0, loc)
		if !day.Before(to) {
			break
		}
		wd := weekdayAbbrev[day.Weekday()]
		for _, b := range sched {
			if !containsString(b.Days, wd) {
				continue
			}
			sm, ok1 := clockMinutes(b.StartLocal)
			em, ok2 := clockMinutes(b.EndLocal)
			if !ok1 || !ok2 || sm == em {
				continue
			}
			endDay := day.Day()
			if em <= sm {
				endDay++
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), sm/60, sm%60, 0, 0, loc)
			end := time.Date(day.Year(), day.Month(), endDay, em/60, em%60, 0, 0, loc)
			if em == 24*60 {
				end = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
			}
			if s, e := maxTime(start, from), minTime(end, to); e.After(s) {
				raw = append(raw, [2]time.Time{s, e})
			}
		}
	}
	return subtractSpans(mergeSpans(raw), nightSpans(from, to, loc))
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func mergeSpans(spans [][2]time.Time) [][2]time.Time {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0].Before(spans[j][0]) })
	var out [][2]time.Time
	for _, s := range spans {
		if n := len(out); n > 0 && !s[0].After(out[n-1][1]) {
			out[n-1][1] = maxTime(out[n-1][1], s[1])
			continue
		}
		out = append(out, s)
	}
	return out
}

func subtractSpans(spans, cut [][2]time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, s := range spans {
		cur := s[0]
		for _, c := range cut {
			if !c[1].After(cur) || !c[0].Before(s[1]) {
				continue
			}
			if c[0].After(cur) {
				out = append(out, [2]time.Time{cur, c[0]})
			}
			cur = maxTime(cur, c[1])
		}
		if s[1].After(cur) {
			out = append(out, [2]time.Time{cur, s[1]})
		}
	}
	return out
}

func awayPriced(item interval, from, to time.Time, tariff marginalTariff, loc *time.Location, sched awaySchedule) *big.Rat {
	total := new(big.Rat)
	for _, s := range awaySpans(maxTime(item.Start, from), minTime(item.End, to), loc, sched) {
		if part := priceIntervalRange(item, s[0], s[1], tariff); part != nil {
			total.Add(total, part)
		}
	}
	return total
}

func awayCoveredShare(energyItems []interval, id string, b bucket, loc *time.Location, sched awaySchedule) *big.Rat {
	covered, away := new(big.Rat), new(big.Rat)
	for _, item := range energyItems {
		if item.ApplianceID != id || item.Unavailable || !terminalDevice(item) {
			continue
		}
		start, end := maxTime(item.Start, b.Start), minTime(item.End, b.End)
		if !end.After(start) {
			continue
		}
		share := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(item.End.Sub(item.Start).Nanoseconds()))
		seconds := new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(int64(time.Second)))
		if item.Covered != nil {
			seconds = new(big.Rat).Mul(item.Covered, share)
		}
		covered.Add(covered, seconds)
		inAway := int64(0)
		for _, s := range awaySpans(start, end, loc, sched) {
			inAway += s[1].Sub(s[0]).Nanoseconds()
		}
		away.Add(away, new(big.Rat).Mul(seconds, big.NewRat(inAway, end.Sub(start).Nanoseconds())))
	}
	if covered.Sign() == 0 {
		return new(big.Rat)
	}
	return away.Quo(away, covered)
}

// awayComponent prices waste inside the default away block; a user schedule
// is applied through awayComponentFor when the caller has loaded one.
func awayComponent(in componentInput) componentResult {
	return awayComponentFor(in, nil)
}

// awayComponentFor returns each metered device's waste cost inside the away
// window outside the night window. A nil schedule uses the 09:00-16:00 default,
// labelled estimated; a user schedule is measured.
func awayComponentFor(in componentInput, user awaySchedule) componentResult {
	sched, source, basis := defaultAwaySchedule, awaySourceDefault, "estimated"
	if user != nil {
		sched, source, basis = user, awaySourceUser, "measured"
	}
	res := componentResult{Devices: map[string]deviceAmount{}, Basis: "insufficient_data", Source: "none"}
	if in.Tariff == nil || in.Loc == nil {
		return res
	}
	res.Source = source
	sums := map[string]*big.Rat{}
	add := func(id string, v *big.Rat) {
		if sums[id] == nil {
			sums[id] = new(big.Rat)
		}
		sums[id].Add(sums[id], v)
	}
	for i, b := range in.Buckets {
		energyItems, costItems := b.deviceItems, b.deviceItems
		if energyItems == nil {
			_, energyItems, _ = selectBucketSources(in.Items, b.Start, b.End)
		}
		if costItems == nil {
			costItems = in.Priced
		}
		for _, item := range costItems {
			spec, ok := in.Specs[item.ApplianceID]
			if !ok || spec.StandbyWatts <= 0 || !terminalDevice(item) {
				continue
			}
			if waste, counted := intervalIsWaste(item, spec.StandbyWatts); counted && waste {
				add(item.ApplianceID, awayPriced(item, b.Start, b.End, *in.Tariff, in.Loc, sched))
			}
		}
		if i >= len(in.Rows) {
			continue
		}
		for _, row := range in.Rows[i] {
			spec, ok := in.Specs[row.ApplianceID]
			if !ok || spec.StandbyWatts > 0 || spec.FloorUSDPerDay <= 0 || row.Allocation.Energy == nil {
				continue
			}
			totalWh := new(big.Rat).Mul(mustDecimal(row.Allocation.Energy.Value), big.NewRat(1000, 1))
			wasteWh := floorWasteWh(spec.FloorUSDPerDay, *in.Tariff, b, energyItems, row.ApplianceID, totalWh)
			piece := interval{Start: b.Start, End: b.End, Wh: wasteWh}
			if cost := priceIntervalRange(piece, b.Start, b.End, *in.Tariff); cost != nil {
				add(row.ApplianceID, new(big.Rat).Mul(cost, awayCoveredShare(energyItems, row.ApplianceID, b, in.Loc, sched)))
			}
		}
	}
	for id, v := range sums {
		res.Devices[id] = deviceAmount{Amount: v, Basis: basis}
	}
	if len(sums) > 0 {
		res.Basis = basis
	}
	return res
}
