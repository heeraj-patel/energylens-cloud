package energyrate

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

// DefaultEIABaseURL is the U.S. EIA Open Data API v2 host.
const DefaultEIABaseURL = "https://api.eia.gov"

// eiaRetailSalesPath is the electricity retail-sales dataset (monthly
// average retail price by state and sector).
const eiaRetailSalesPath = "/v2/electricity/retail-sales/data/"

// ErrNoData means EIA answered but had no usable price for the region.
var ErrNoData = errors.New("energyrate: eia returned no price data")

// Rate is one region's average residential price for one EIA data month.
type Rate struct {
	Region    string    // two-letter state code, or NationalRegion
	USDPerKWh float64   // dollars per kWh
	Period    time.Time // first day of the EIA data month (UTC)
	FetchedAt time.Time // when the service read it from EIA
}

// NationalRegion is EIA's stateid for the U.S. total.
const NationalRegion = "US"

// RateFetcher reads the latest residential average price for a region.
type RateFetcher interface {
	LatestResidentialPrice(ctx context.Context, region string) (Rate, error)
}

// EIAClient calls the EIA Open Data API v2. The API key travels only in the
// query string of outbound requests; it is never logged and is scrubbed from
// returned errors (net/http's *url.Error embeds the full request URL).
type EIAClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Now     func() time.Time
}

// NewEIAClient builds a client with a bounded timeout. baseURL "" selects
// DefaultEIABaseURL.
func NewEIAClient(baseURL, apiKey string) *EIAClient {
	if baseURL == "" {
		baseURL = DefaultEIABaseURL
	}
	return &EIAClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		Now:     time.Now,
	}
}

// eiaResponse is the subset of the v2 envelope we read. EIA v2.1 serializes
// numeric data values as JSON strings ("15.8"), older responses as numbers,
// and missing months as null — eiaNumber accepts all three.
type eiaResponse struct {
	Response struct {
		Data []struct {
			Period  string    `json:"period"`
			StateID string    `json:"stateid"`
			Sector  string    `json:"sectorid"`
			Price   eiaNumber `json:"price"`
			Units   string    `json:"price-units"`
		} `json:"data"`
	} `json:"response"`
}

type eiaNumber struct {
	v     float64
	valid bool
}

func (n *eiaNumber) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		return nil
	}
	s = strings.Trim(s, `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // unparseable value: treat as missing, not a hard error
	}
	n.v, n.valid = f, true
	return nil
}

// LatestResidentialPrice implements RateFetcher: the most recent month with a
// non-null residential (sectorid=RES) price for region.
func (c *EIAClient) LatestResidentialPrice(ctx context.Context, region string) (Rate, error) {
	if c.APIKey == "" {
		return Rate{}, errors.New("energyrate: no EIA API key configured")
	}
	q := url.Values{}
	q.Set("api_key", c.APIKey)
	q.Set("frequency", "monthly")
	q.Set("data[0]", "price")
	q.Set("facets[stateid][]", region)
	q.Set("facets[sectorid][]", "RES")
	q.Set("sort[0][column]", "period")
	q.Set("sort[0][direction]", "desc")
	q.Set("offset", "0")
	// A few months, so a null latest month (EIA sometimes publishes the
	// row before the value) still yields the previous one.
	q.Set("length", "6")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+eiaRetailSalesPath+"?"+q.Encode(), nil)
	if err != nil {
		return Rate{}, fmt.Errorf("energyrate: build eia request: %w", c.scrub(err))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Rate{}, fmt.Errorf("energyrate: eia request: %w", c.scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Rate{}, fmt.Errorf("energyrate: read eia response: %w", c.scrub(err))
	}
	if resp.StatusCode != http.StatusOK {
		return Rate{}, fmt.Errorf("energyrate: eia status %d", resp.StatusCode)
	}
	var er eiaResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return Rate{}, fmt.Errorf("energyrate: decode eia response: %w", err)
	}
	for _, d := range er.Response.Data {
		if !d.Price.valid || d.Price.v <= 0 {
			continue
		}
		if d.StateID != "" && !strings.EqualFold(d.StateID, region) {
			continue
		}
		if d.Sector != "" && !strings.EqualFold(d.Sector, "RES") {
			continue
		}
		period, err := time.Parse("2006-01", d.Period)
		if err != nil {
			continue
		}
		return Rate{
			Region:    region,
			USDPerKWh: centsToUSD(d.Price.v, d.Units),
			Period:    period.UTC(),
			FetchedAt: c.Now().UTC(),
		}, nil
	}
	return Rate{}, ErrNoData
}

// centsToUSD converts EIA's "cents per kilowatt-hour" to USD/kWh, rounded to
// 4 decimals (0.01 cent). A unit string that already says dollars is left
// as-is.
func centsToUSD(v float64, units string) float64 {
	u := strings.ToLower(units)
	if !strings.Contains(u, "cent") && strings.Contains(u, "dollar") {
		return math.Round(v*10000) / 10000
	}
	return math.Round(v*100) / 10000
}

// scrub removes the API key from an error's text. *url.Error carries the full
// request URL (including api_key=...), so it is unwrapped to its cause, and
// any remaining occurrence of the key is masked as a belt-and-braces measure.
func (c *EIAClient) scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = fmt.Errorf("%s %s: %w", ue.Op, c.BaseURL+eiaRetailSalesPath, ue.Err)
	}
	if c.APIKey != "" && strings.Contains(err.Error(), c.APIKey) {
		return errors.New(strings.ReplaceAll(err.Error(), c.APIKey, "REDACTED"))
	}
	return err
}
