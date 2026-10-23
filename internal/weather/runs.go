package weather

import (
	"sort"
	"time"
)

const (
	hotAboveF      = 98
	coldBelowF     = 39
	runActiveDays  = 11
	runMaxStaleDay = 3
	// SourceObserved marks a stored day as an observation rather than a forecast.
	SourceObserved = "observed"
)

// Day is one stored local day; nil High or Low means no value.
type Day struct {
	Date   string
	Source string
	HighF  *float64
	LowF   *float64
}

// Run is a streak of extreme days ending at LastDay, the newest day in it.
type Run struct {
	Days    int
	LastDay string
	Active  bool
}

// Runs counts hot and cold streaks over observed days up to lastCompleted
// (YYYY-MM-DD). Days without a value are skipped; a run is active at 11 days
// and only when its newest observed day is within the last 3 completed days.
func Runs(days []Day, lastCompleted string) (hot, cold Run) {
	end, err := time.Parse("2006-01-02", lastCompleted)
	if err != nil {
		return Run{}, Run{}
	}
	oldest := end.AddDate(0, 0, 1-runMaxStaleDay).Format("2006-01-02")
	obs := make([]Day, 0, len(days))
	for _, d := range days {
		if d.Source == SourceObserved && d.Date <= lastCompleted {
			obs = append(obs, d)
		}
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Date > obs[j].Date })
	hot = countRun(obs, oldest, func(d Day) (bool, bool) {
		if d.HighF == nil {
			return false, false
		}
		return *d.HighF > hotAboveF, true
	})
	cold = countRun(obs, oldest, func(d Day) (bool, bool) {
		if d.LowF == nil {
			return false, false
		}
		return *d.LowF < coldBelowF, true
	})
	return hot, cold
}

func countRun(newestFirst []Day, oldestFresh string, test func(Day) (match, present bool)) Run {
	var r Run
	seen := false
	for _, d := range newestFirst {
		match, present := test(d)
		if !present {
			continue
		}
		if !seen {
			seen = true
			if d.Date < oldestFresh {
				return Run{}
			}
		}
		if !match {
			break
		}
		if r.Days == 0 {
			r.LastDay = d.Date
		}
		r.Days++
	}
	r.Active = r.Days >= runActiveDays
	return r
}
