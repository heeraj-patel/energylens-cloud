package householdenergy

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// touPeriod is one tariff_revision_periods adder row; times are home-local seconds of day, end may be 86400.
// When end < start the row wraps: it covers [start, 24:00) and [00:00, end) of each listed day.
type touPeriod struct {
	Adder      *big.Rat
	Days       map[time.Weekday]bool
	Start, End int
	Months     map[time.Month]bool
}

func (p touPeriod) covers(month time.Month, day time.Weekday, second int) bool {
	if !p.Months[month] || !p.Days[day] {
		return false
	}
	if p.Start < p.End {
		return second >= p.Start && second < p.End
	}
	return second >= p.Start || second < p.End
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func secondsOfDay(raw string) (int, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("invalid period time %q", raw)
	}
	total := 0
	for i, unit := range []int{3600, 60, 1} {
		v, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid period time %q", raw)
		}
		total += int(v) * unit
	}
	return total, nil
}

func parsePeriod(adder string, days []string, start, end string, months []int16) (touPeriod, error) {
	p := touPeriod{Days: map[time.Weekday]bool{}, Months: map[time.Month]bool{}}
	var ok bool
	if p.Adder, ok = new(big.Rat).SetString(adder); !ok {
		return p, fmt.Errorf("invalid period adder")
	}
	for _, d := range days {
		day, known := weekdays[d]
		if !known {
			return p, fmt.Errorf("invalid period day %q", d)
		}
		p.Days[day] = true
	}
	for _, m := range months {
		p.Months[time.Month(m)] = true
	}
	var err error
	if p.Start, err = secondsOfDay(start); err != nil {
		return p, err
	}
	p.End, err = secondsOfDay(end)
	return p, err
}

// periodsOverlap reports whether two rows of one revision price the same local (month, day, second).
func periodsOverlap(periods []touPeriod) bool {
	spans := func(p touPeriod) [][2]int {
		if p.Start < p.End {
			return [][2]int{{p.Start, p.End}}
		}
		return [][2]int{{p.Start, 86400}, {0, p.End}}
	}
	for i := range periods {
		for j := i + 1; j < len(periods); j++ {
			a, b := periods[i], periods[j]
			shared := false
			for m := range a.Months {
				for d := range a.Days {
					shared = shared || b.Months[m] && b.Days[d]
				}
			}
			if !shared {
				continue
			}
			for _, x := range spans(a) {
				for _, y := range spans(b) {
					if x[0] < y[1] && y[0] < x[1] {
						return true
					}
				}
			}
		}
	}
	return false
}

// readTariffPeriods attaches each revision's adder rows and the home time zone they are read in.
func readTariffPeriods(ctx context.Context, tx pgx.Tx, home string, segments []marginalTariff) error {
	if len(segments) == 0 {
		return nil
	}
	index := map[string]int{}
	ids := make([]string, len(segments))
	for i, s := range segments {
		index[s.ID], ids[i] = i, s.ID
	}
	rows, err := tx.Query(ctx, `SELECT tariff_revision_id::text,adder::text,days,start_local::text,end_local::text,months
 FROM tariff_revision_periods WHERE tariff_revision_id=ANY($1::uuid[]) ORDER BY tariff_revision_id,ordinal`, ids)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var id, adder, start, end string
		var days []string
		var months []int16
		if err = rows.Scan(&id, &adder, &days, &start, &end, &months); err != nil {
			rows.Close()
			return err
		}
		p, err := parsePeriod(adder, days, start, end, months)
		if err != nil {
			rows.Close()
			return err
		}
		segments[index[id]].Periods = append(segments[index[id]].Periods, p)
		found = true
	}
	rows.Close()
	if err = rows.Err(); err != nil || !found {
		return err
	}
	var tz string
	if err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1`, home).Scan(&tz); err != nil {
		return err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("home time zone: %w", err)
	}
	for i := range segments {
		if periodsOverlap(segments[i].Periods) {
			return fmt.Errorf("tariff revision %s has overlapping periods", segments[i].ID)
		}
		segments[i].Loc = loc
	}
	return nil
}

type pricedPiece struct {
	from, to time.Time
	rate     *big.Rat
}

// rateAt is the revision rate plus the adder of the row covering t's home-local wall clock, or plus 0.
func (t *marginalTariff) rateAt(at time.Time) *big.Rat {
	local := at.In(t.Loc)
	second := local.Hour()*3600 + local.Minute()*60 + local.Second()
	for _, p := range t.Periods {
		if p.covers(local.Month(), local.Weekday(), second) {
			return new(big.Rat).Add(t.Rate, p.Adder)
		}
	}
	return t.Rate
}

// pricedPieces splits [from, to) where the local price changes; it steps by wall-clock boundaries and zone changes, so DST days keep local hours.
func (t *marginalTariff) pricedPieces(from, to time.Time) []pricedPiece {
	if len(t.Periods) == 0 || t.Loc == nil {
		return []pricedPiece{{from, to, t.Rate}}
	}
	bounds := map[int]bool{86400: true}
	for _, p := range t.Periods {
		bounds[p.Start], bounds[p.End] = true, true
	}
	sorted := make([]int, 0, len(bounds))
	for b := range bounds {
		sorted = append(sorted, b)
	}
	sort.Ints(sorted)
	pieces := []pricedPiece{}
	for cur := from; cur.Before(to); {
		local := cur.In(t.Loc)
		sec := local.Hour()*3600 + local.Minute()*60 + local.Second()
		next := sorted[sort.SearchInts(sorted, sec+1)]
		step := time.Duration(next-sec)*time.Second - time.Duration(local.Nanosecond())
		end := cur.Add(step)
		if _, zoneEnd := local.ZoneBounds(); !zoneEnd.IsZero() && zoneEnd.After(cur) && zoneEnd.Before(end) {
			end = zoneEnd
		}
		if end.After(to) {
			end = to
		}
		rate := t.rateAt(cur)
		if n := len(pieces); n > 0 && pieces[n-1].rate.Cmp(rate) == 0 {
			pieces[n-1].to = end
		} else {
			pieces = append(pieces, pricedPiece{cur, end, rate})
		}
		cur = end
	}
	return pieces
}
