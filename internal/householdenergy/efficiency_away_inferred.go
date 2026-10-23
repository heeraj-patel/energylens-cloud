package householdenergy

import (
	"math/big"
	"sort"
	"time"
)

// Inferred away (spec 1.2 option C, D-E3): an hour is near baseline when its
// home draw is at most awayNearBaseline x the p10 of the trailing 7 local days.
var awayNearBaseline = big.NewRat(6, 5)

const (
	awayBaselineDays     = 7
	awayBaselineMinHours = 24
	awayMinRunHours      = 2
)

// homeHour is one home-local clock hour with its average whole-home draw in W.
type homeHour struct {
	Start, End time.Time
	Day        int64
	Watts      *big.Rat
	Night      bool
}

func civilDay(t time.Time, loc *time.Location) int64 {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// homeHours returns the measured hours of [from,to) from the same source
// selection as the home total; hours under half covered are skipped.
func homeHours(items []interval, from, to time.Time, loc *time.Location) []homeHour {
	l := from.In(loc)
	var out []homeHour
	for start := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc); start.Before(to); start = start.Add(time.Hour) {
		end := start.Add(time.Hour)
		total, _, _ := selectBucketSources(items, start, end)
		wh, covered := new(big.Rat), time.Duration(0)
		for _, v := range total {
			if v.Unavailable || v.Wh == nil || !v.End.After(v.Start) {
				continue
			}
			s, e := maxTime(v.Start, start), minTime(v.End, end)
			if !e.After(s) {
				continue
			}
			covered += e.Sub(s)
			wh.Add(wh, new(big.Rat).Mul(v.Wh, big.NewRat(e.Sub(s).Nanoseconds(), v.End.Sub(v.Start).Nanoseconds())))
		}
		if covered < 30*time.Minute {
			continue
		}
		watts := wh.Mul(wh, big.NewRat(int64(time.Hour), covered.Nanoseconds()))
		out = append(out, homeHour{Start: start, End: end, Day: civilDay(start, loc), Watts: watts, Night: len(nightSpans(start, end, loc)) > 0})
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

// inferAwaySpans returns runs of 2+ consecutive near-baseline hours outside the
// night window. A day needs 24 measured hours in its 7-day window and at least
// one hour above the threshold, so a flat draw is never read as away.
func inferAwaySpans(in componentInput) [][2]time.Time {
	if in.Loc == nil || len(in.Buckets) == 0 {
		return nil
	}
	hours := homeHours(in.Items, in.Buckets[0].Start, in.Buckets[len(in.Buckets)-1].End, in.Loc)
	thresholds := map[int64]*big.Rat{}
	for _, h := range hours {
		if _, done := thresholds[h.Day]; done {
			continue
		}
		var window []*big.Rat
		var peak *big.Rat
		for _, w := range hours {
			if w.Day <= h.Day && w.Day > h.Day-awayBaselineDays {
				window = append(window, w.Watts)
				if peak == nil || w.Watts.Cmp(peak) > 0 {
					peak = w.Watts
				}
			}
		}
		thresholds[h.Day] = nil
		if len(window) < awayBaselineMinHours {
			continue
		}
		if limit := new(big.Rat).Mul(p10(window), awayNearBaseline); peak.Cmp(limit) > 0 {
			thresholds[h.Day] = limit
		}
	}
	var spans [][2]time.Time
	var run []homeHour
	flush := func() {
		if len(run) >= awayMinRunHours {
			spans = append(spans, [2]time.Time{run[0].Start, run[len(run)-1].End})
		}
		run = nil
	}
	for _, h := range hours {
		limit := thresholds[h.Day]
		if h.Night || limit == nil || h.Watts.Cmp(limit) > 0 {
			flush()
			continue
		}
		if n := len(run); n > 0 && !run[n-1].End.Equal(h.Start) {
			flush()
		}
		run = append(run, h)
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
