package weather

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NWS reads api.weather.gov points, gridpoint forecasts and active alerts.
type NWS struct {
	BaseURL   string // tests only; production leaves this empty
	UserAgent string
	Client    *http.Client
}

type nwsPoint struct {
	Office  string
	X, Y    int
	Station string
}

var nwsOfficePattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Heat and cold event names from https://www.weather.gov/safety/heat-ww and https://www.weather.gov/safety/cold-ww (checked 2026-10-23).
var nwsAlertKinds = map[string]string{
	"Heat Advisory": KindHot, "Extreme Heat Watch": KindHot, "Extreme Heat Warning": KindHot,
	"Cold Weather Advisory": KindCold, "Extreme Cold Watch": KindCold, "Extreme Cold Warning": KindCold,
	"Freeze Watch": KindCold, "Freeze Warning": KindCold,
}

func (p NWS) base() string {
	if p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return "https://api.weather.gov"
}

func (p NWS) fetch(ctx context.Context, rawURL string, out any) (http.Header, error) {
	return getJSON(ctx, p.Client, rawURL, p.UserAgent, "application/geo+json", out)
}

func coord(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64)
}

// point resolves the forecast grid and nearest observation station for a centroid.
func (p NWS) point(ctx context.Context, lat, lon float64, now time.Time) (nwsPoint, time.Time, bool, error) {
	var body struct {
		Properties struct {
			GridID              string `json:"gridId"`
			GridX               *int   `json:"gridX"`
			GridY               *int   `json:"gridY"`
			ObservationStations string `json:"observationStations"`
		} `json:"properties"`
	}
	h, err := p.fetch(ctx, p.base()+"/points/"+coord(lat)+","+coord(lon), &body)
	if err != nil {
		return nwsPoint{}, time.Time{}, false, err
	}
	pr := body.Properties
	if !nwsOfficePattern.MatchString(pr.GridID) || pr.GridX == nil || pr.GridY == nil || *pr.GridX < 0 || *pr.GridY < 0 {
		return nwsPoint{}, time.Time{}, false, fmt.Errorf("%w: NWS point without grid", ErrInvalidSourceData)
	}
	pt := nwsPoint{Office: pr.GridID, X: *pr.GridX, Y: *pr.GridY}
	exp, ok := cacheExpiry(h, now)
	if pr.ObservationStations == "" {
		return pt, exp, ok, nil
	}
	if !strings.HasPrefix(pr.ObservationStations, p.base()+"/") {
		return nwsPoint{}, time.Time{}, false, fmt.Errorf("%w: NWS stations link outside API", ErrInvalidSourceData)
	}
	var stations struct {
		Features []struct {
			Properties struct {
				StationIdentifier string `json:"stationIdentifier"`
			} `json:"properties"`
		} `json:"features"`
	}
	sh, err := p.fetch(ctx, pr.ObservationStations, &stations)
	if err != nil {
		return nwsPoint{}, time.Time{}, false, err
	}
	if len(stations.Features) > 0 {
		pt.Station = strings.TrimSpace(stations.Features[0].Properties.StationIdentifier)
	}
	sexp, sok := cacheExpiry(sh, now)
	exp, ok = earliest(exp, ok, sexp, sok)
	return pt, exp, ok, nil
}

// forecast maps day periods to that local date's high and night periods to the
// low of the local date the night ends on, the morning the low is observed.
func (p NWS) forecast(ctx context.Context, pt nwsPoint, tz *time.Location, now time.Time) ([]forecastDay, string, time.Time, bool, error) {
	ref := fmt.Sprintf("%s/gridpoints/%s/%d,%d/forecast", p.base(), pt.Office, pt.X, pt.Y)
	var body struct {
		Properties struct {
			Periods []struct {
				StartTime       string   `json:"startTime"`
				EndTime         string   `json:"endTime"`
				IsDaytime       bool     `json:"isDaytime"`
				Temperature     *float64 `json:"temperature"`
				TemperatureUnit string   `json:"temperatureUnit"`
			} `json:"periods"`
		} `json:"properties"`
	}
	h, err := p.fetch(ctx, ref, &body)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	days := newDayBuilder()
	for _, period := range body.Properties.Periods {
		if period.Temperature == nil {
			continue
		}
		f := *period.Temperature
		switch period.TemperatureUnit {
		case "F":
		case "C":
			f = celsiusToF(f)
		default:
			return nil, "", time.Time{}, false, fmt.Errorf("%w: NWS temperature unit %q", ErrInvalidSourceData, period.TemperatureUnit)
		}
		at := period.StartTime
		if !period.IsDaytime {
			at = period.EndTime
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, "", time.Time{}, false, fmt.Errorf("%w: NWS period time", ErrInvalidSourceData)
		}
		if err := days.add(t.In(tz).Format(dateLayout), f, period.IsDaytime); err != nil {
			return nil, "", time.Time{}, false, err
		}
	}
	if len(days.days) == 0 {
		return nil, "", time.Time{}, false, fmt.Errorf("%w: NWS forecast without temperatures", ErrInvalidSourceData)
	}
	exp, ok := cacheExpiry(h, now)
	return days.list(), ref, exp, ok, nil
}

func (p NWS) alerts(ctx context.Context, lat, lon float64, now time.Time) ([]alertRecord, time.Time, bool, error) {
	var body struct {
		Features []struct {
			ID         string `json:"id"`
			Properties struct {
				ID          string  `json:"id"`
				URL         string  `json:"@id"`
				Event       string  `json:"event"`
				Headline    *string `json:"headline"`
				Status      string  `json:"status"`
				MessageType string  `json:"messageType"`
				Effective   string  `json:"effective"`
				Onset       string  `json:"onset"`
				Expires     string  `json:"expires"`
				Ends        string  `json:"ends"`
			} `json:"properties"`
		} `json:"features"`
	}
	h, err := p.fetch(ctx, p.base()+"/alerts/active?point="+url.QueryEscape(coord(lat)+","+coord(lon)), &body)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	out := make([]alertRecord, 0, len(body.Features))
	for _, f := range body.Features {
		pr := f.Properties
		if pr.Status != "Actual" || pr.MessageType == "Cancel" || strings.TrimSpace(pr.Event) == "" || pr.ID == "" {
			continue
		}
		starts, ok := firstTime(pr.Onset, pr.Effective)
		if !ok {
			continue
		}
		ends, ok := firstTime(pr.Ends, pr.Expires)
		if !ok || !ends.After(starts) {
			continue
		}
		link := pr.URL
		if link == "" {
			link = f.ID
		}
		if !strings.HasPrefix(link, "https://") {
			link = "https://api.weather.gov/alerts/" + url.PathEscape(pr.ID)
		}
		out = append(out, alertRecord{SourceID: pr.ID, Event: pr.Event, Headline: nonEmpty(pr.Headline), StartsAt: starts, EndsAt: ends, SourceURL: link, Kind: nwsAlertKinds[pr.Event]})
	}
	exp, ok := cacheExpiry(h, now)
	return out, exp, ok, nil
}

func firstTime(values ...string) (time.Time, bool) {
	for _, v := range values {
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func nonEmpty(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}
