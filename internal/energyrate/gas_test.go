package energyrate

import (
	"context"
	"errors"
	"testing"
	"time"
)

var gasNow = time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)

func gasRate(region string, perTherm float64, fetched time.Time) GasRate {
	return GasRate{Region: region, USDPerTherm: perTherm, Period: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), FetchedAt: fetched}
}

func gasService(c GasCache, static GasStaticRate) *Service {
	return NewService(Options{GasCache: c, GasStatic: static, Now: func() time.Time { return gasNow }})
}

func gasCacheWith(rates ...GasRate) *MemGasCache {
	c := NewMemGasCache()
	for _, r := range rates {
		c.Put(r)
	}
	return c
}

type failingGasCache struct{}

func (failingGasCache) Get(context.Context, string) (GasRate, bool, error) {
	return GasRate{}, false, errors.New("db down")
}

func TestResolveGasStateRow(t *testing.T) {
	c := gasCacheWith(gasRate("TX", 1.5, gasNow), gasRate(NationalRegion, 1.4, gasNow))
	got, err := gasService(c, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.USDPerTherm != 1.5 || got.Region == nil || *got.Region != "TX" || got.IsStale {
		t.Fatalf("state row not used: %+v err=%v", got, err)
	}
	if got.AsOf != "2026-07-01" || got.FetchedAt == nil || !got.FetchedAt.Equal(gasNow) {
		t.Fatalf("provenance lost: %+v", got)
	}
}

func TestResolveGasFallsBackToUSRow(t *testing.T) {
	c := gasCacheWith(gasRate(NationalRegion, 1.4, gasNow))
	got, err := gasService(c, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisNational || got.USDPerTherm != 1.4 || got.Region != nil {
		t.Fatalf("US row not used: %+v err=%v", got, err)
	}
}

func TestResolveGasStaleRowWithinMaxSourceAge(t *testing.T) {
	c := gasCacheWith(gasRate("TX", 1.6, gasNow.Add(-48*time.Hour)))
	s := NewService(Options{GasCache: c, CacheTTL: 24 * time.Hour, Now: func() time.Time { return gasNow }})
	got, err := s.ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.USDPerTherm != 1.6 || !got.IsStale {
		t.Fatalf("stale row not served: %+v err=%v", got, err)
	}
}

func TestResolveGasExpiredRowUsesStatic(t *testing.T) {
	static := GasStaticRate{USDPerTherm: 1.481, AsOf: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), SourceURL: "https://example.test"}
	c := gasCacheWith(gasRate("TX", 1.6, gasNow.Add(-MaxSourceAge-time.Second)))
	got, err := gasService(c, static).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisStatic || got.USDPerTherm != 1.481 || got.FetchedAt != nil {
		t.Fatalf("expired row not replaced by static: %+v err=%v", got, err)
	}
}

func TestResolveGasQuarterOldRowIsFresh(t *testing.T) {
	c := gasCacheWith(gasRate("TX", 1.6, gasNow.Add(-92*24*time.Hour)))
	got, err := gasService(c, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisState || got.IsStale {
		t.Fatalf("row from the last quarterly load not served fresh: %+v err=%v", got, err)
	}
}

func TestResolveGasReadErrorUsesStatic(t *testing.T) {
	static := GasStaticRate{USDPerTherm: 1.481, AsOf: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	got, err := gasService(failingGasCache{}, static).ResolveGas(context.Background(), "78732")
	if err != nil || got.Basis != GasBasisStatic {
		t.Fatalf("read error did not fall back to static: %+v err=%v", got, err)
	}
}

func TestResolveGasUnavailable(t *testing.T) {
	_, err := gasService(nil, GasStaticRate{}).ResolveGas(context.Background(), "78732")
	if !errors.Is(err, ErrTemporarilyUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
