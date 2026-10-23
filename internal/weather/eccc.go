package weather

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ECCC reads MSC GeoMet citypageweather-realtime forecasts and weather-alerts.
type ECCC struct {
	BaseURL   string // tests only; production leaves this empty
	UserAgent string
	Client    *http.Client
}

// Attribution is the wording the ECCC licence requires next to its data.
const Attribution = "Data Source: Environment and Climate Change Canada"

// Heat and cold alert names from canada.ca .../severeweather/extreme-heat-warning.html and .../extreme-cold-warning.html (checked 2026-10-23).
var ecccAlertKinds = map[string]string{
	"heat warning": KindHot, "cold warning": KindCold, "extreme cold warning": KindCold,
}

var ecccSitePattern = regexp.MustCompile(`^[a-z]{2}-[0-9]{1,4}$`)

var ecccWeekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

func (p ECCC) base() string {
	if p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return "https://api.weather.gc.ca"
}

func (p ECCC) fetch(ctx context.Context, rawURL string, out any) (http.Header, error) {
	return getJSON(ctx, p.Client, rawURL, p.UserAgent, "application/json", out)
}

func bbox(lat, lon, d float64) string {
	return coord(lon-d) + "," + coord(lat-d) + "," + coord(lon+d) + "," + coord(lat+d)
}

// site finds the citypage site nearest the centroid, widening the search once.
func (p ECCC) site(ctx context.Context, lat, lon float64) (string, error) {
	for _, d := range []float64{1, 3} {
		var body struct {
			Features []struct {
				ID       string `json:"id"`
				Geometry struct {
					Coordinates []float64 `json:"coordinates"`
				} `json:"geometry"`
			} `json:"features"`
		}
		q := url.Values{"f": {"json"}, "bbox": {bbox(lat, lon, d)}, "properties": {"identifier"}, "limit": {"500"}}
		if _, err := p.fetch(ctx, p.base()+"/collections/citypageweather-realtime/items?"+q.Encode(), &body); err != nil {
			return "", err
		}
		best, bestDist := "", math.Inf(1)
		for _, f := range body.Features {
			if !ecccSitePattern.MatchString(f.ID) || len(f.Geometry.Coordinates) < 2 {
				continue
			}
			dx := (f.Geometry.Coordinates[0] - lon) * math.Cos(lat*math.Pi/180)
			dy := f.Geometry.Coordinates[1] - lat
			if dist := dx*dx + dy*dy; dist < bestDist {
				best, bestDist = f.ID, dist
			}
		}
		if best != "" {
			return best, nil
		}
	}
	return "", fmt.Errorf("%w: no ECCC citypage site near centroid", ErrNoProviderPoint)
}

type ecccText struct {
	En any `json:"en"`
}

func (t ecccText) text() string {
	s, _ := t.En.(string)
	return strings.TrimSpace(s)
}

// forecast dates each period by its weekday name on or after the issue date;
// a night low belongs to the next local date, as for NWS.
func (p ECCC) forecast(ctx context.Context, site string, tz *time.Location, now time.Time) ([]forecastDay, string, time.Time, bool, error) {
	ref := p.base() + "/collections/citypageweather-realtime/items/" + site
	var body struct {
		Properties struct {
			LastUpdated   string `json:"lastUpdated"`
			ForecastGroup struct {
				Timestamp ecccText `json:"timestamp"`
				Forecasts []struct {
					Period struct {
						Value ecccText `json:"value"`
					} `json:"period"`
					Temperatures struct {
						Temperature []struct {
							Class ecccText `json:"class"`
							Units ecccText `json:"units"`
							Value struct {
								En *float64 `json:"en"`
							} `json:"value"`
						} `json:"temperature"`
					} `json:"temperatures"`
				} `json:"forecasts"`
			} `json:"forecastGroup"`
		} `json:"properties"`
	}
	h, err := p.fetch(ctx, ref+"?f=json", &body)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	fg := body.Properties.ForecastGroup
	issued, ok := firstTime(fg.Timestamp.text(), body.Properties.LastUpdated)
	if !ok {
		return nil, "", time.Time{}, false, fmt.Errorf("%w: ECCC forecast without issue time", ErrInvalidSourceData)
	}
	local := issued.In(tz)
	cursor := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	days := newDayBuilder()
	for _, f := range fg.Forecasts {
		name := strings.ToLower(f.Period.Value.text())
		night := strings.HasSuffix(name, " night")
		wd, known := ecccWeekdays[strings.TrimSuffix(name, " night")]
		if !known {
			return nil, "", time.Time{}, false, fmt.Errorf("%w: ECCC period %q", ErrInvalidSourceData, name)
		}
		steps := (int(wd) - int(cursor.Weekday()) + 7) % 7
		cursor = cursor.AddDate(0, 0, steps)
		for _, t := range f.Temperatures.Temperature {
			class := t.Class.text()
			if t.Value.En == nil || (class == "high") == night || (class != "high" && class != "low") {
				continue
			}
			if t.Units.text() != "C" {
				return nil, "", time.Time{}, false, fmt.Errorf("%w: ECCC temperature unit", ErrInvalidSourceData)
			}
			date := cursor
			if night {
				date = cursor.AddDate(0, 0, 1)
			}
			if err := days.add(date.Format(dateLayout), celsiusToF(*t.Value.En), !night); err != nil {
				return nil, "", time.Time{}, false, err
			}
		}
	}
	if len(days.days) == 0 {
		return nil, "", time.Time{}, false, fmt.Errorf("%w: ECCC forecast without temperatures", ErrInvalidSourceData)
	}
	exp, cacheable := cacheExpiry(h, now)
	return days.list(), ref, exp, cacheable, nil
}

func (p ECCC) alerts(ctx context.Context, lat, lon float64, now time.Time) ([]alertRecord, time.Time, bool, error) {
	var body struct {
		Features []struct {
			ID         string `json:"id"`
			Properties struct {
				AlertNameEn         string `json:"alert_name_en"`
				StatusEn            string `json:"status_en"`
				PublicationDatetime string `json:"publication_datetime"`
				ValidityDatetime    string `json:"validity_datetime"`
				EventEndDatetime    string `json:"event_end_datetime"`
				ExpirationDatetime  string `json:"expiration_datetime"`
			} `json:"properties"`
		} `json:"features"`
	}
	q := url.Values{"f": {"json"}, "bbox": {bbox(lat, lon, 0.001)}, "limit": {"500"}}
	h, err := p.fetch(ctx, p.base()+"/collections/weather-alerts/items?"+q.Encode(), &body)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	seen := map[string]bool{}
	out := make([]alertRecord, 0, len(body.Features))
	for _, f := range body.Features {
		pr := f.Properties
		status := strings.ToLower(strings.TrimSpace(pr.StatusEn))
		if f.ID == "" || seen[f.ID] || strings.TrimSpace(pr.AlertNameEn) == "" || status == "ended" || status == "cancelled" {
			continue
		}
		starts, ok := firstTime(pr.ValidityDatetime, pr.PublicationDatetime)
		if !ok {
			continue
		}
		ends, ok := firstTime(pr.EventEndDatetime, pr.ExpirationDatetime)
		if !ok || !ends.After(starts) {
			continue
		}
		seen[f.ID] = true
		out = append(out, alertRecord{SourceID: f.ID, Event: pr.AlertNameEn, StartsAt: starts, EndsAt: ends,
			SourceURL: p.base() + "/collections/weather-alerts/items/" + url.PathEscape(f.ID), Kind: ecccAlertKinds[strings.ToLower(strings.TrimSpace(pr.AlertNameEn))]})
	}
	exp, cacheable := cacheExpiry(h, now)
	return out, exp, cacheable, nil
}
