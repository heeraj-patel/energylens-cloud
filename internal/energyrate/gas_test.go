package energyrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeGasFetcher struct {
	rates map[string]GasRate
	calls int
}

func (f *fakeGasFetcher) ResidentialGasPrice(_ context.Context, region string) (GasRate, error) {
	f.calls++
	if r, ok := f.rates[region]; ok {
		return r, nil
	}
	return GasRate{}, ErrNoData
}

var gasNow = time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)

func gasRate(region string, perTherm float64, fetched time.Time) GasRate {
	return GasRate{Region: region, USDPerTherm: perTherm, Period: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), FetchedAt: fetched}
}

func gasService(f GasRateFetcher, hasKey bool, c *GasCache, static GasStaticRate) *Service {
	return NewService(Options{GasFetcher: f, HasKey: hasKey, GasCache: c, GasStatic: static, Now: func() time.Time { return gasNow }})
}

func TestResolveGasStateRow(t *testing.T) {
	f := &fakeGasFetcher{rates: map[string]GasRate{"TX": gasRate("TX", 1.5, gasNow), NationalRegion: gasRate(NationalRegion, 1.4, gasNow)}}
	got, err := gasService(f, true, nil, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.USDPerTherm != 1.5 || got.Region == nil || *got.Region != "TX" || got.IsStale {
		t.Fatalf("state row not used: %+v err=%v", got, err)
	}
}

func TestResolveGasFallsBackToUSRow(t *testing.T) {
	f := &fakeGasFetcher{rates: map[string]GasRate{NationalRegion: gasRate(NationalRegion, 1.4, gasNow)}}
	got, err := gasService(f, true, nil, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisNational || got.USDPerTherm != 1.4 || got.Region != nil {
		t.Fatalf("US row not used: %+v err=%v", got, err)
	}
}

func TestResolveGasStaleCacheWhenEIAFails(t *testing.T) {
	cache := NewGasCache()
	cache.put(gasRate("TX", 1.6, gasNow.Add(-48*time.Hour*15)))
	f := &fakeGasFetcher{}
	got, err := gasService(f, true, cache, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.USDPerTherm != 1.6 || !got.IsStale {
		t.Fatalf("stale cache not served: %+v err=%v", got, err)
	}
}

func TestResolveGasNoKeyUsesCacheThenStatic(t *testing.T) {
	static := GasStaticRate{USDPerTherm: 1.481, AsOf: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), SourceURL: "https://example.test"}
	f := &fakeGasFetcher{rates: map[string]GasRate{"TX": gasRate("TX", 1.5, gasNow)}}
	s := gasService(f, false, nil, static)
	got, err := s.ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisStatic || got.USDPerTherm != 1.481 || f.calls != 0 || got.FetchedAt != nil {
		t.Fatalf("no-key did not use static without fetching: %+v calls=%d err=%v", got, f.calls, err)
	}
	cache := NewGasCache()
	cache.put(gasRate("TX", 1.7, gasNow.Add(-time.Hour)))
	got, err = gasService(f, false, cache, static).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.USDPerTherm != 1.7 {
		t.Fatalf("no-key ignored fresh cache: %+v err=%v", got, err)
	}
}

func TestResolveGasUnavailable(t *testing.T) {
	_, err := gasService(nil, false, nil, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if !errors.Is(err, ErrTemporarilyUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func gasBody(n int, value string) string {
	rows := ""
	for i := 0; i < n; i++ {
		p := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -i, 0)
		if i > 0 {
			rows += ","
		}
		rows += fmt.Sprintf(`{"period":"%s","series":"N3010TX3","value":%s}`, p.Format("2006-01"), value)
	}
	return `{"response":{"data":[` + rows + `]}}`
}

func gasClient(t *testing.T, body string, query *string) *EIAClient {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if query != nil {
			*query = r.URL.Path + "?" + r.URL.RawQuery
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewEIAClient(srv.URL, "k")
	c.Now = func() time.Time { return gasNow }
	return c
}

func TestEIAGasTwelveMonthMeanAndConversion(t *testing.T) {
	var q string
	// $/Mcf strings (EIA v2 style); mean 15.34 -> 15.34 / 10.36 = 1.4807 $/therm.
	r, err := gasClient(t, gasBody(12, `"15.34"`), &q).ResidentialGasPrice(context.Background(), "TX")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.USDPerTherm-1.4807) > 1e-9 || !r.Period.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) || r.Region != "TX" {
		t.Fatalf("rate = %+v", r)
	}
	for _, want := range []string{"/v2/natural-gas/pri/sum/data/", "length=12", "frequency=monthly"} {
		if !strings.Contains(q, want) {
			t.Fatalf("request %q lacks %q", q, want)
		}
	}
}

func TestEIAGasMeanOfDistinctMonths(t *testing.T) {
	vals := []float64{10, 20, 10, 20, 10, 20, 10, 20, 10, 20, 10, 20}
	rows := ""
	for i, v := range vals {
		p := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -i, 0)
		if i > 0 {
			rows += ","
		}
		rows += fmt.Sprintf(`{"period":"%s","value":%v}`, p.Format("2006-01"), v)
	}
	r, err := gasClient(t, `{"response":{"data":[`+rows+`]}}`, nil).ResidentialGasPrice(context.Background(), "TX")
	if err != nil || math.Abs(r.USDPerTherm-math.Round(15/10.36*10000)/10000) > 1e-9 {
		t.Fatalf("mean wrong: %+v err=%v", r, err)
	}
}

func TestEIAGasFewerThanTwelveMonthsIsNoData(t *testing.T) {
	for name, body := range map[string]string{
		"11 rows": gasBody(11, `"15"`),
		"null":    gasBody(12, `null`),
	} {
		if _, err := gasClient(t, body, nil).ResidentialGasPrice(context.Background(), "TX"); !errors.Is(err, ErrNoData) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestEIAGasNoKey(t *testing.T) {
	c := NewEIAClient("http://127.0.0.1:1", "")
	if _, err := c.ResidentialGasPrice(context.Background(), "TX"); err == nil {
		t.Fatal("expected no-key error")
	}
}
