package householdenergy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOUDay is one local day's time-of-use spread: the saving per kWh from moving
// use out of the day's peak window to its lowest-adder hour (same rule as peakSaving).
type TOUDay struct {
	Currency     string
	SavingPerKWh *big.Rat
	PeakStart    string // home-local HH:MM
	PeakEnd      string
}

// TOUDay reads the home's electricity tariff for the local day of now; nil when
// the home has no fresh tariff with TOU periods or the day has no price spread.
func (s Service) TOUDay(ctx context.Context, owner, home string, now time.Time) (*TOUDay, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var tz string
	err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("home time zone: %w", err)
	}
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	electricity := "electricity"
	day := []interval{{Start: start, End: time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc), Wh: new(big.Rat), Fuel: &electricity}}
	_, _, schedule, err := readEffectiveTariff(ctx, tx, home, now, day)
	if err != nil || schedule == nil {
		return nil, err
	}
	return touDayOf(tariffAt(now, schedule.Segments), now), nil
}

func touDayOf(t *marginalTariff, at time.Time) *TOUDay {
	if t == nil || t.Loc == nil || len(t.Periods) == 0 || t.Currency != "USD" {
		return nil
	}
	local := at.In(t.Loc)
	bounds := map[int]bool{0: true, 86400: true}
	for _, p := range t.Periods {
		bounds[p.Start], bounds[p.End] = true, true
	}
	cuts := make([]int, 0, len(bounds))
	for b := range bounds {
		cuts = append(cuts, b)
	}
	sort.Ints(cuts)
	type span struct {
		from, to int
		adder    *big.Rat
	}
	spans := make([]span, 0, len(cuts))
	var high, low *big.Rat
	for i := 0; i+1 < len(cuts); i++ {
		adder := new(big.Rat)
		for _, p := range t.Periods {
			if p.covers(local.Month(), local.Weekday(), cuts[i]) {
				adder = p.Adder
				break
			}
		}
		spans = append(spans, span{cuts[i], cuts[i+1], adder})
		if high == nil || adder.Cmp(high) > 0 {
			high = adder
		}
		if low == nil || adder.Cmp(low) < 0 {
			low = adder
		}
	}
	if high == nil || high.Cmp(low) <= 0 {
		return nil
	}
	runs := [][2]int{}
	for _, s := range spans {
		if s.adder.Cmp(high) != 0 {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1][1] == s.from {
			runs[n-1][1] = s.to
		} else {
			runs = append(runs, [2]int{s.from, s.to})
		}
	}
	peak := runs[0]
	if n := len(runs); n > 1 && runs[0][0] == 0 && runs[n-1][1] == 86400 {
		peak = [2]int{runs[n-1][0], runs[0][1]}
	}
	return &TOUDay{Currency: t.Currency, SavingPerKWh: new(big.Rat).Sub(high, low), PeakStart: clock(peak[0]), PeakEnd: clock(peak[1])}
}

func clock(second int) string {
	second %= 86400
	return fmt.Sprintf("%02d:%02d", second/3600, second%3600/60)
}
