package weather

import (
	"testing"
	"time"
)

const lastDone = "2026-10-22"

func f(v float64) *float64 { return &v }

func dayAt(back int, source string, hi, lo *float64) Day {
	t, _ := time.Parse("2006-01-02", lastDone)
	return Day{Date: t.AddDate(0, 0, -back).Format("2006-01-02"), Source: source, HighF: hi, LowF: lo}
}

func series(from, n int, hi, lo *float64) []Day {
	var out []Day
	for i := from; i < from+n; i++ {
		out = append(out, dayAt(i, SourceObserved, hi, lo))
	}
	return out
}

func TestRuns(t *testing.T) {
	tests := []struct {
		name      string
		days      []Day
		hotDays   int
		hotActive bool
		coldDays  int
		coldAct   bool
		hotLast   string
	}{
		{"10 hot days not active", series(0, 10, f(100), f(70)), 10, false, 0, false, "2026-10-22"},
		{"11 hot days active", series(0, 11, f(100), f(70)), 11, true, 0, false, "2026-10-22"},
		{"boundary high 98 is not hot", series(0, 12, f(98), f(70)), 0, false, 0, false, ""},
		{"high 98.1 is hot", series(0, 11, f(98.1), f(70)), 11, true, 0, false, "2026-10-22"},
		{"boundary low 39 is not cold", series(0, 12, f(60), f(39)), 0, false, 0, false, ""},
		{"low 38.9 is cold", series(0, 11, f(60), f(38.9)), 0, false, 11, true, ""},
		{"mild day resets run", append(series(0, 5, f(100), f(70)), append([]Day{dayAt(5, SourceObserved, f(80), f(60))}, series(6, 10, f(100), f(70))...)...), 5, false, 0, false, "2026-10-22"},
		{"gap day inside run neither breaks nor extends", append(series(0, 5, f(100), f(70)), series(6, 6, f(100), f(70))...), 11, true, 0, false, "2026-10-22"},
		{"day with nil values skipped", append(series(0, 5, f(100), f(70)), append([]Day{dayAt(5, SourceObserved, nil, nil)}, series(6, 6, f(100), f(70))...)...), 11, true, 0, false, "2026-10-22"},
		{"forecast days ignored", append(series(0, 6, f(100), f(70)), func() []Day {
			var o []Day
			for i := 6; i < 12; i++ {
				o = append(o, dayAt(i, "forecast", f(100), f(70)))
			}
			return o
		}()...), 6, false, 0, false, "2026-10-22"},
		{"forecast day inside run is skipped", append(series(0, 5, f(100), f(70)), append([]Day{dayAt(5, "forecast", f(50), f(20))}, series(6, 6, f(100), f(70))...)...), 11, true, 0, false, "2026-10-22"},
		{"future and today days ignored", append(series(0, 11, f(100), f(70)), Day{Date: "2026-10-23", Source: SourceObserved, HighF: f(50), LowF: f(20)}), 11, true, 0, false, "2026-10-22"},
		{"stale newest day within 3 completed days counts", series(2, 11, f(100), f(70)), 11, true, 0, false, "2026-10-20"},
		{"stale newest day beyond 3 completed days dropped", series(3, 11, f(100), f(70)), 0, false, 0, false, ""},
		{"hot and cold independent", func() []Day {
			var o []Day
			for i := 0; i < 11; i++ {
				o = append(o, dayAt(i, SourceObserved, f(100), f(30)))
			}
			return o
		}(), 11, true, 11, true, "2026-10-22"},
		{"cold run with mild highs while hot broken", append(series(0, 11, f(60), f(30)), dayAt(11, SourceObserved, f(100), f(50))), 0, false, 11, true, ""},
		{"unsorted input", func() []Day {
			d := series(0, 11, f(100), f(70))
			d[0], d[10] = d[10], d[0]
			return d
		}(), 11, true, 0, false, "2026-10-22"},
		{"no days", nil, 0, false, 0, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hot, cold := Runs(tc.days, lastDone)
			if hot.Days != tc.hotDays || hot.Active != tc.hotActive {
				t.Fatalf("hot = %+v, want days %d active %v", hot, tc.hotDays, tc.hotActive)
			}
			if tc.hotLast != "" && hot.LastDay != tc.hotLast {
				t.Fatalf("hot last day = %q, want %q", hot.LastDay, tc.hotLast)
			}
			if cold.Days != tc.coldDays || cold.Active != tc.coldAct {
				t.Fatalf("cold = %+v, want days %d active %v", cold, tc.coldDays, tc.coldAct)
			}
		})
	}
}

func TestRunsBadLastCompleted(t *testing.T) {
	hot, cold := Runs(series(0, 11, f(100), f(30)), "bad")
	if hot != (Run{}) || cold != (Run{}) {
		t.Fatalf("got %+v %+v", hot, cold)
	}
}
