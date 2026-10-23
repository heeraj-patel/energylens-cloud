package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"energylens.local/cloud/internal/datawizfacts"

	"github.com/jackc/pgx/v5"
)

const (
	CapabilityPeakDemand = "peak_demand_v1"

	contextPeakDemandDay = "peak_demand_day"
	peakDemandWindow     = "13:00-19:00"
	maxPeakHomeTips      = 3
)

// PeakDemandPage is the response's peak_demand block.
type PeakDemandPage struct {
	Status      string              `json:"status"`
	WindowLocal string              `json:"window_local"`
	SourceURL   string              `json:"source_url"`
	HomeTips    []PeakDemandHomeTip `json:"home_tips"`
}

type PeakDemandHomeTip struct {
	Fact datawizfacts.Fact `json:"fact"`
}

// PeakDemandHome is the home's latest chosen utility and its local time.
type PeakDemandHome struct {
	EIAID string
	Local time.Time
}

// peakDemandStore is implemented by stores that hold the peak_demand_day tips.
type peakDemandStore interface {
	PeakDemandTips(context.Context) ([]datawizfacts.Fact, error)
	PeakDemandHome(ctx context.Context, owner, home string, now time.Time) (PeakDemandHome, error)
}

// PeakDemandTips reads the peak_demand_day tips; only peak_demand_v1 asks for them.
func (s PGStore) PeakDemandTips(ctx context.Context) ([]datawizfacts.Fact, error) {
	return efficiencyTips(ctx, s.Pool, []string{contextPeakDemandDay})
}

func (s PGStore) PeakDemandHome(ctx context.Context, owner, home string, now time.Time) (PeakDemandHome, error) {
	var tz string
	err := s.Pool.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return PeakDemandHome{}, ErrNotFound
	}
	if err != nil {
		return PeakDemandHome{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return PeakDemandHome{}, err
	}
	var eiaid string
	err = s.Pool.QueryRow(ctx, `SELECT eiaid FROM home_electric_utility WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&eiaid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PeakDemandHome{}, err
	}
	return PeakDemandHome{EIAID: eiaid, Local: now.In(loc)}, nil
}

// inPeakSeason is June 1 to September 30 by the home's local date.
func inPeakSeason(local time.Time) bool {
	m := local.Month()
	return m >= time.June && m <= time.September
}

type peakTipValue struct {
	Context    string   `json:"context"`
	Applicable []string `json:"applicable_categories"`
	Providers  []string `json:"applicable_provider_ids"`
	Sentence   string   `json:"action_source_text"`
}

func peakTipOf(tip datawizfacts.Fact, eiaid string) (peakTipValue, bool) {
	var v peakTipValue
	if tip.Field != "efficiency_tip" || tip.ID == "" || json.Unmarshal(tip.Value, &v) != nil ||
		v.Context != contextPeakDemandDay || !slices.Contains(v.Providers, eiaid) {
		return v, false
	}
	return v, true
}

func peakSourceURL(tip datawizfacts.Fact) string {
	var src struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(tip.Source, &src) != nil || !strings.HasPrefix(src.URL, "https://") {
		return ""
	}
	return src.URL
}

// peakDemandView builds the block from the tips that apply to the utility; nil
// when none has an https source. Home tips are deduped by sentence.
func peakDemandView(tips []datawizfacts.Fact, eiaid string) (*PeakDemandPage, []datawizfacts.Fact) {
	page := &PeakDemandPage{Status: "available", WindowLocal: peakDemandWindow, HomeTips: []PeakDemandHomeTip{}}
	var eligible []datawizfacts.Fact
	seen := map[string]bool{}
	for _, tip := range tips {
		v, ok := peakTipOf(tip, eiaid)
		url := peakSourceURL(tip)
		if !ok || url == "" {
			continue
		}
		if page.SourceURL == "" {
			page.SourceURL = url
		}
		eligible = append(eligible, tip)
		key := v.Sentence
		if key == "" {
			key = tip.ID
		}
		if slices.Contains(v.Applicable, "home") && !seen[key] && len(page.HomeTips) < maxPeakHomeTips {
			seen[key] = true
			page.HomeTips = append(page.HomeTips, PeakDemandHomeTip{Fact: tip})
		}
	}
	if page.SourceURL == "" {
		return nil, nil
	}
	return page, eligible
}

// withPeakDemand adds the peak_demand block and places the peak_demand_day
// tips right after any weather tips. Ineligible homes are untouched.
func (s Service) withPeakDemand(ctx context.Context, owner, home string, now time.Time, appliances []Appliance) ([]Appliance, *PeakDemandPage, error) {
	store, ok := s.Store.(peakDemandStore)
	if !ok {
		return appliances, nil, nil
	}
	h, err := store.PeakDemandHome(ctx, owner, home, now)
	if err != nil || h.EIAID == "" || !inPeakSeason(h.Local) {
		return appliances, nil, err
	}
	tips, err := store.PeakDemandTips(ctx)
	if err != nil {
		return nil, nil, err
	}
	page, eligible := peakDemandView(tips, h.EIAID)
	if page == nil {
		return appliances, nil, nil
	}
	out := make([]Appliance, len(appliances))
	for i, a := range appliances {
		at := 0
		for at < len(a.ResearchTips) {
			if v, ok := parseTip(a.ResearchTips[at]); !ok || !isWeatherContext(v.Context) {
				break
			}
			at++
		}
		a.ResearchTips = slices.Concat(a.ResearchTips[:at], eligible, a.ResearchTips[at:])
		out[i] = a
	}
	return out, page, nil
}
