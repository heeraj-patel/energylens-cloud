package weather

import (
	"net/http"
	"testing"
	"time"
)

func TestCacheExpiry(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		headers []string
		want    time.Duration
		reused  bool
	}{
		{"s-maxage wins over max-age", []string{"public, max-age=60, s-maxage=3600"}, time.Hour, true},
		{"max-age alone", []string{"public, max-age=86400"}, 24 * time.Hour, true},
		{"quoted s-maxage", []string{`s-maxage="30"`}, 30 * time.Second, true},
		{"split headers", []string{"public", "max-age=5"}, 5 * time.Second, true},
		{"no-store", []string{"no-store, max-age=600"}, 0, false},
		{"no-cache", []string{"no-cache"}, 0, false},
		{"absent", nil, 0, false},
		{"zero lifetime", []string{"max-age=0"}, 0, false},
		{"garbage lifetime", []string{"max-age=soon"}, 0, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.headers {
			h.Add("Cache-Control", v)
		}
		got, ok := cacheExpiry(h, now)
		if ok != c.reused || (ok && !got.Equal(now.Add(c.want))) {
			t.Errorf("%s: got %v %t", c.name, got, ok)
		}
	}
}

func TestEarliestNeedsBothCacheable(t *testing.T) {
	a, b := time.Unix(100, 0), time.Unix(50, 0)
	if got, ok := earliest(a, true, b, true); !ok || !got.Equal(b) {
		t.Fatalf("earliest: %v %t", got, ok)
	}
	if _, ok := earliest(a, true, b, false); ok {
		t.Fatal("uncacheable part was reused")
	}
}
