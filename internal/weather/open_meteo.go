package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Coordinate is a reviewed centroid for a coarse ZIP3/FSA cell, never a home address.
type Coordinate struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// OpenMeteo uses the commercial customer endpoint. Callers must explicitly
// supply licensed credentials and a reviewed coarse-cell centroid map.
type OpenMeteo struct {
	APIKey   string
	Cells    map[string]Coordinate
	Client   *http.Client
	Endpoint string // tests only; production leaves this empty
}

func (p OpenMeteo) Name() string { return "open-meteo-commercial" }

func (p OpenMeteo) FetchDaily(ctx context.Context, cell, timezone, day string) (float64, string, error) {
	if p.APIKey == "" {
		return 0, "", errors.New("weather provider unconfigured")
	}
	c, ok := p.Cells[cell]
	if !ok || math.IsNaN(c.Latitude) || math.IsNaN(c.Longitude) || c.Latitude < -90 || c.Latitude > 90 || c.Longitude < -180 || c.Longitude > 180 {
		return 0, "", errors.New("coarse cell unmapped")
	}
	if _, err := time.LoadLocation(timezone); err != nil || timezone == "Local" {
		return 0, "", errors.New("invalid timezone")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return 0, "", errors.New("invalid day")
	}
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = "https://customer-archive-api.open-meteo.com/v1/archive"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, "", err
	}
	q := u.Query()
	q.Set("latitude", strconv.FormatFloat(c.Latitude, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(c.Longitude, 'f', 4, 64))
	q.Set("start_date", day)
	q.Set("end_date", day)
	q.Set("daily", "temperature_2m_mean")
	q.Set("timezone", timezone)
	q.Set("temperature_unit", "celsius")
	q.Set("apikey", p.APIKey)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", err
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("weather provider HTTP %d", response.StatusCode)
	}
	var body struct {
		Timezone string `json:"timezone"`
		Daily    struct {
			Time []string   `json:"time"`
			Mean []*float64 `json:"temperature_2m_mean"`
		} `json:"daily"`
		DailyUnits struct {
			Mean string `json:"temperature_2m_mean"`
		} `json:"daily_units"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return 0, "", err
	}
	if body.Timezone != timezone || body.DailyUnits.Mean != "°C" || len(body.Daily.Time) != 1 || body.Daily.Time[0] != day || len(body.Daily.Mean) != 1 || body.Daily.Mean[0] == nil {
		return 0, "", errors.New("incomplete weather provider response")
	}
	mean := *body.Daily.Mean[0]
	if math.IsNaN(mean) || math.IsInf(mean, 0) || mean < -100 || mean > 70 {
		return 0, "", errors.New("invalid weather temperature")
	}
	return mean, "open-meteo-archive-best-match-v1", nil
}

// ParseCoarseCells fails closed on malformed or fine-grained mapping keys.
func ParseCoarseCells(raw string) (map[string]Coordinate, error) {
	var cells map[string]Coordinate
	if err := json.Unmarshal([]byte(raw), &cells); err != nil {
		return nil, err
	}
	if len(cells) == 0 {
		return nil, errors.New("no coarse weather cells configured")
	}
	for key, c := range cells {
		parts := strings.Split(key, ":")
		if len(parts) != 3 || ((parts[0] != "US" || len(parts[1]) != 3) && (parts[0] != "CA" || len(parts[1]) != 3)) || math.IsNaN(c.Latitude) || math.IsNaN(c.Longitude) || c.Latitude < -90 || c.Latitude > 90 || c.Longitude < -180 || c.Longitude > 180 {
			return nil, fmt.Errorf("invalid coarse cell mapping %q", key)
		}
		if parts[0] == "US" {
			for _, r := range parts[1] {
				if r < '0' || r > '9' {
					return nil, fmt.Errorf("invalid ZIP3 %q", key)
				}
			}
		}
		if parts[0] == "CA" {
			for i, r := range parts[1] {
				if (i%2 == 0 && (r < 'A' || r > 'Z')) || (i%2 == 1 && (r < '0' || r > '9')) {
					return nil, fmt.Errorf("invalid FSA %q", key)
				}
			}
		}
		if _, err := time.LoadLocation(parts[2]); err != nil {
			return nil, err
		}
	}
	return cells, nil
}
