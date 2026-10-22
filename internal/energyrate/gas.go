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
	"strings"
	"sync"
	"time"
)

// eiaGasPricePath is the natural gas price summary dataset; residential
// monthly series are named N3010<ST>3 ($/Mcf).
const eiaGasPricePath = "/v2/natural-gas/pri/sum/data/"

// mcfPerTherm converts $/Mcf to $/therm: 1 ft3 = 1,036 Btu, 1 therm = 100,000 Btu.
const mcfPerTherm = 10.36

// GasMonths is how many of the latest monthly periods are requested for the mean.
const GasMonths = 12

// Gas rate bases (stored later as gas_rate_basis).
const (
	GasBasisState    = "state_average"
	GasBasisNational = "national_average"
	GasBasisStatic   = "static"
)

// GasRate is one region's mean residential gas price over the latest 12 months.
type GasRate struct {
	Region      string    // two-letter state code, or NationalRegion
	USDPerTherm float64   // mean of the months with data, converted from $/Mcf
	Period      time.Time // first day of the latest month with data (UTC)
	FetchedAt   time.Time
}

// GasRateFetcher reads the 12-month mean residential gas price for a region.
type GasRateFetcher interface {
	ResidentialGasPrice(ctx context.Context, region string) (GasRate, error)
}

// GasStaticRate is the cited US value used when EIA and the cache cannot supply one.
type GasStaticRate struct {
	USDPerTherm float64 // <= 0 disables the static fallback
	AsOf        time.Time
	SourceURL   string
}

// GasResolved is a resolved gas rate with its provenance.
type GasResolved struct {
	USDPerTherm float64
	Basis       string // GasBasis*
	Region      *string
	AsOf        string
	FetchedAt   *time.Time
	IsStale     bool
	SourceURL   string
}

type eiaGasResponse struct {
	Response struct {
		Data []struct {
			Period string    `json:"period"`
			Series string    `json:"series"`
			Value  eiaNumber `json:"value"`
		} `json:"data"`
	} `json:"response"`
}

// ResidentialGasPrice implements GasRateFetcher. It averages the months with
// data among the latest 12; none at all is ErrNoData.
func (c *EIAClient) ResidentialGasPrice(ctx context.Context, region string) (GasRate, error) {
	if c.APIKey == "" {
		return GasRate{}, errors.New("energyrate: no EIA API key configured")
	}
	q := url.Values{}
	q.Set("api_key", c.APIKey)
	q.Set("frequency", "monthly")
	q.Set("data[0]", "value")
	q.Set("facets[series][]", gasSeries(region))
	q.Set("sort[0][column]", "period")
	q.Set("sort[0][direction]", "desc")
	q.Set("offset", "0")
	q.Set("length", fmt.Sprint(GasMonths))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+eiaGasPricePath+"?"+q.Encode(), nil)
	if err != nil {
		return GasRate{}, fmt.Errorf("energyrate: build eia request: %w", c.scrubPath(err, eiaGasPricePath))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return GasRate{}, fmt.Errorf("energyrate: eia request: %w", c.scrubPath(err, eiaGasPricePath))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return GasRate{}, fmt.Errorf("energyrate: read eia response: %w", c.scrubPath(err, eiaGasPricePath))
	}
	if resp.StatusCode != http.StatusOK {
		return GasRate{}, fmt.Errorf("energyrate: eia status %d", resp.StatusCode)
	}
	var er eiaGasResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return GasRate{}, fmt.Errorf("energyrate: decode eia response: %w", err)
	}
	return gasMean(er, region, c.Now().UTC())
}

func gasSeries(region string) string { return "N3010" + strings.ToUpper(region) + "3" }

// gasMean averages the non-null, positive values among the rows (newest first),
// dividing by the months that had data. No such month is ErrNoData.
func gasMean(er eiaGasResponse, region string, now time.Time) (GasRate, error) {
	var sum float64
	var latest time.Time
	n, rows := 0, 0
	for _, d := range er.Response.Data {
		if d.Series != "" && !strings.EqualFold(d.Series, gasSeries(region)) {
			continue
		}
		if rows++; rows > GasMonths {
			break
		}
		period, err := time.Parse("2006-01", d.Period)
		if err != nil || !d.Value.valid || d.Value.v <= 0 {
			continue
		}
		if n == 0 {
			latest = period
		}
		sum += d.Value.v
		n++
	}
	if n == 0 {
		return GasRate{}, ErrNoData
	}
	return GasRate{
		Region:      region,
		USDPerTherm: math.Round(sum/float64(n)/mcfPerTherm*10000) / 10000,
		Period:      latest.UTC(),
		FetchedAt:   now,
	}, nil
}

// GasCache stores the last gas rate per region; in memory only (no table yet).
type GasCache struct {
	mu sync.RWMutex
	m  map[string]GasRate
}

// NewGasCache returns an empty gas rate cache.
func NewGasCache() *GasCache { return &GasCache{m: map[string]GasRate{}} }

func (c *GasCache) get(region string) (GasRate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[region]
	return r, ok
}

func (c *GasCache) put(r GasRate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[r.Region] = r
}

// ResolveGas returns the gas rate for a US ZIP ("" means national): EIA state
// mean, else EIA US mean, else the static value, else ErrTemporarilyUnavailable.
func (s *Service) ResolveGas(ctx context.Context, zip string) (GasResolved, error) {
	if st := StateForZIP(zip); st != "" {
		if r, stale, ok := s.gasRegionRate(ctx, st); ok {
			region := st
			return GasResolved{USDPerTherm: r.USDPerTherm, Basis: GasBasisState, Region: &region,
				AsOf: r.Period.Format(time.DateOnly), FetchedAt: &r.FetchedAt, IsStale: stale}, nil
		}
	}
	if r, stale, ok := s.gasRegionRate(ctx, NationalRegion); ok {
		return GasResolved{USDPerTherm: r.USDPerTherm, Basis: GasBasisNational,
			AsOf: r.Period.Format(time.DateOnly), FetchedAt: &r.FetchedAt, IsStale: stale}, nil
	}
	if s.gasStatic.USDPerTherm > 0 {
		return GasResolved{USDPerTherm: s.gasStatic.USDPerTherm, Basis: GasBasisStatic,
			AsOf: s.gasStatic.AsOf.Format(time.DateOnly), SourceURL: s.gasStatic.SourceURL}, nil
	}
	return GasResolved{}, ErrTemporarilyUnavailable
}

// gasRegionRate mirrors regionRate: fresh cache, else fetch, else stale cache
// within MaxSourceAge. A failed fetch backs off for failureBackoff.
func (s *Service) gasRegionRate(ctx context.Context, region string) (GasRate, bool, bool) {
	cached, found := s.gasCache.get(region)
	age := s.now().Sub(cached.FetchedAt)
	valid := found && !cached.FetchedAt.IsZero() && age >= 0 && age <= MaxSourceAge
	if valid && age < s.ttl {
		return cached, false, true
	}
	if s.gasHasKey {
		key := "gas:" + region
		s.mu.Lock()
		backoff := s.now().Before(s.failUntil[key])
		s.mu.Unlock()
		if !backoff {
			fresh, err := s.fetchGas(ctx, region)
			if err == nil {
				return fresh, false, true
			}
			s.log.Warn("energyrate: EIA gas fetch failed", "region", region, "err", err)
		}
	}
	return cached, valid, valid
}

// fetchGas calls EIA with per-region request coalescing, records the failure
// backoff, and writes the cache.
func (s *Service) fetchGas(ctx context.Context, region string) (GasRate, error) {
	s.mu.Lock()
	if c, ok := s.gasInflight[region]; ok {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.r, c.err
		case <-ctx.Done():
			return GasRate{}, ctx.Err()
		}
	}
	c := &gasCall{done: make(chan struct{})}
	s.gasInflight[region] = c
	s.mu.Unlock()

	c.r, c.err = s.gasFetcher.ResidentialGasPrice(context.WithoutCancel(ctx), region)
	if c.err == nil {
		if c.r.FetchedAt.IsZero() {
			c.r.FetchedAt = s.now().UTC()
		}
		s.gasCache.put(c.r)
	}
	s.mu.Lock()
	delete(s.gasInflight, region)
	if c.err != nil {
		s.failUntil["gas:"+region] = s.now().Add(failureBackoff)
	} else {
		delete(s.failUntil, "gas:"+region)
	}
	s.mu.Unlock()
	close(c.done)
	return c.r, c.err
}
