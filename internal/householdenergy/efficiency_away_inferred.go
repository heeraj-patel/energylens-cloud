package householdenergy

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Inferred away (spec 1.2 option C, D-E3), whole-home meter only: a measured
// interval is low when its draw is at most the trailing 7-day p10 plus the home's
// reviewed standby W, or awayNearBaseline x p10 when no device has a standby fact.
var awayNearBaseline = big.NewRat(6, 5)

const (
	awayBaselineDays = 7
	awayMinSpan      = 2 * time.Hour
)

// homeDraw is one stretch of the whole-home meter covered by the same measured
// intervals, with its average draw in W.
type homeDraw struct {
	Start, End time.Time
	Day        int64
	Watts      *big.Rat
	key        string
}

func civilDay(t time.Time, loc *time.Location) int64 {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func sourceKey(v interval) string {
	return strings.Join([]string{v.HubID, v.SensorID, v.EventID, strconv.FormatInt(v.Revision, 10), strconv.FormatInt(v.Start.UnixNano(), 10), strconv.FormatInt(v.End.UnixNano(), 10)}, "|")
}

func bucketWindows(buckets []bucket) [][2]time.Time {
	out := make([][2]time.Time, len(buckets))
	for i, b := range buckets {
		out[i] = [2]time.Time{b.Start, b.End}
	}
	return out
}

func hourWindows(from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	for s := from; s.Before(to); s = s.Add(time.Hour) {
		out = append(out, [2]time.Time{s, minTime(s.Add(time.Hour), to)})
	}
	return out
}

// homeDraws returns the whole-home meter draws in each window; windows whose
// home total is not a whole-home meter yield none.
func homeDraws(items []interval, windows [][2]time.Time, loc *time.Location) []homeDraw {
	var out []homeDraw
	for _, w := range windows {
		total, _, whole := selectBucketSources(items, w[0], w[1])
		if !whole {
			continue
		}
		var sources []interval
		var cuts []time.Time
		for _, v := range total {
			if v.Unavailable || v.Wh == nil || !v.End.After(v.Start) {
				continue
			}
			if s, e := maxTime(v.Start, w[0]), minTime(v.End, w[1]); e.After(s) {
				sources = append(sources, v)
				cuts = append(cuts, s, e)
			}
		}
		sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
		for i := 0; i+1 < len(cuts); i++ {
			s, e := cuts[i], cuts[i+1]
			if !e.After(s) {
				continue
			}
			watts := new(big.Rat)
			var keys []string
			for _, v := range sources {
				if v.Start.After(s) || v.End.Before(e) {
					continue
				}
				watts.Add(watts, new(big.Rat).Mul(v.Wh, big.NewRat(int64(time.Hour), v.End.Sub(v.Start).Nanoseconds())))
				keys = append(keys, sourceKey(v))
			}
			if len(keys) == 0 {
				continue
			}
			sort.Strings(keys)
			key := strings.Join(keys, ",")
			if n := len(out); n > 0 && out[n-1].End.Equal(s) && out[n-1].key == key {
				out[n-1].End = e
				continue
			}
			out = append(out, homeDraw{Start: s, End: e, Day: civilDay(s, loc), Watts: watts, key: key})
		}
	}
	return out
}

// p10 is the nearest-rank 10th percentile.
func p10(values []*big.Rat) *big.Rat {
	sorted := append([]*big.Rat(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cmp(sorted[j]) < 0 })
	rank := (len(sorted) + 9) / 10
	return sorted[rank-1]
}

// inferAwaySpans returns runs of low intervals outside the night window spanning
// 2+ hours. Gaps in data neither end a run nor shorten it; a measured non-low
// interval or the night window does. The baseline also reads in.Lookback, the 7
// days before the period. A window with no interval above threshold is flat.
func inferAwaySpans(in componentInput) [][2]time.Time {
	if in.Loc == nil || len(in.Buckets) == 0 {
		return nil
	}
	from := in.Buckets[0].Start
	draws := append(homeDraws(in.Lookback, hourWindows(from.In(in.Loc).AddDate(0, 0, -awayBaselineDays), from), in.Loc), homeDraws(in.Items, bucketWindows(in.Buckets), in.Loc)...)
	standby := new(big.Rat)
	for _, w := range in.Standby {
		standby.Add(standby, w)
	}
	limit := func(base *big.Rat) *big.Rat {
		if len(in.Standby) == 0 {
			return new(big.Rat).Mul(base, awayNearBaseline)
		}
		return new(big.Rat).Add(base, standby)
	}
	limits := map[int64]*big.Rat{}
	for _, d := range draws {
		if _, done := limits[d.Day]; done {
			continue
		}
		limits[d.Day] = nil
		var watts []*big.Rat
		var peak *big.Rat
		for _, w := range draws {
			if w.Day <= d.Day && w.Day > d.Day-awayBaselineDays {
				watts = append(watts, w.Watts)
				if peak == nil || w.Watts.Cmp(peak) > 0 {
					peak = w.Watts
				}
			}
		}
		if l := limit(p10(watts)); peak.Cmp(l) > 0 {
			limits[d.Day] = l
		}
	}
	var spans [][2]time.Time
	var run *[2]time.Time
	flush := func() {
		if run != nil && run[1].Sub(run[0]) >= awayMinSpan {
			spans = append(spans, *run)
		}
		run = nil
	}
	for _, d := range draws {
		if l := limits[d.Day]; l == nil || d.Watts.Cmp(l) > 0 {
			flush()
			continue
		}
		for _, p := range subtractSpans([][2]time.Time{{d.Start, d.End}}, nightSpans(d.Start, d.End, in.Loc)) {
			if run != nil && len(nightSpans(run[1], p[0], in.Loc)) > 0 {
				flush()
			}
			if run == nil {
				run = &[2]time.Time{p[0], p[1]}
			} else {
				run[1] = p[1]
			}
		}
	}
	flush()
	return spans
}

func inferredWindow(spans [][2]time.Time) awayWindow {
	return func(from, to time.Time) [][2]time.Time {
		var out [][2]time.Time
		for _, s := range spans {
			if a, b := maxTime(s[0], from), minTime(s[1], to); b.After(a) {
				out = append(out, [2]time.Time{a, b})
			}
		}
		return out
	}
}

// standbyFact is one reviewed standby_power fact joined to a home appliance by
// category, manufacturer, model, fuel and variant.
type standbyFact struct {
	ApplianceID, Model, MatchQuality, DocumentedScope, MatchedModel, Unit, Value string
}

// standbyWatts keeps confirmed-exact W facts whose documented scope and matched
// model equal the appliance model, as usagefactapi Find does. An appliance with
// two candidate facts is ambiguous and dropped.
func standbyWatts(facts []standbyFact) map[string]*big.Rat {
	out, seen := map[string]*big.Rat{}, map[string]int{}
	for _, f := range facts {
		if f.Unit != "W" || f.MatchQuality != "confirmed_exact" || !strings.EqualFold(f.DocumentedScope, f.Model) || !strings.EqualFold(f.MatchedModel, f.Model) {
			continue
		}
		v, ok := new(big.Rat).SetString(f.Value)
		if !ok || v.Sign() < 0 {
			continue
		}
		if seen[f.ApplianceID]++; seen[f.ApplianceID] == 1 {
			out[f.ApplianceID] = v
		} else {
			delete(out, f.ApplianceID)
		}
	}
	return out
}

// loadStandbyWatts returns the reviewed standby W of each active appliance in the
// home, metered or not, since inference reads only a whole-home draw.
func loadStandbyWatts(ctx context.Context, q rowsQuerier, home string) (map[string]*big.Rat, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,a.identity->>'full_model',f.match_quality,f.documented_scope,f.matched_model,f.unit,f.value::text
 FROM appliances a JOIN active_reviewed_usage_facts f ON f.category=a.category AND lower(f.manufacturer)=lower(a.identity->>'brand')
 AND lower(f.model)=lower(a.identity->>'full_model') AND f.fuel=COALESCE(a.identity->>'fuel','electricity') AND lower(f.variant)=lower(a.identity->>'variant')
 WHERE a.home_id=$1 AND a.archived_at IS NULL AND f.measurement_kind='standby_power' ORDER BY a.id,f.id`, home)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []standbyFact
	for rows.Next() {
		var f standbyFact
		if err = rows.Scan(&f.ApplianceID, &f.Model, &f.MatchQuality, &f.DocumentedScope, &f.MatchedModel, &f.Unit, &f.Value); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return standbyWatts(facts), nil
}
