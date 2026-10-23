package weather

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cacheExpiry reads s-maxage, else max-age, from Cache-Control. A response
// with no-store, no-cache, no lifetime or a zero lifetime is not reused.
func cacheExpiry(h http.Header, now time.Time) (time.Time, bool) {
	var maxAge, sMaxAge int64 = -1, -1
	for _, raw := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(raw, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
			name = strings.ToLower(strings.TrimSpace(name))
			value = strings.Trim(strings.TrimSpace(value), `"`)
			switch name {
			case "no-store", "no-cache":
				return time.Time{}, false
			case "max-age":
				maxAge = parseSeconds(value)
			case "s-maxage":
				sMaxAge = parseSeconds(value)
			}
		}
	}
	seconds := sMaxAge
	if seconds < 0 {
		seconds = maxAge
	}
	if seconds <= 0 {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second).UTC(), true
}

func parseSeconds(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 7*24*3600 {
		return -1
	}
	return n
}

// earliest keeps the sooner of two optional expiries; either missing means not reused.
func earliest(a time.Time, aok bool, b time.Time, bok bool) (time.Time, bool) {
	if !aok || !bok {
		return time.Time{}, false
	}
	if b.Before(a) {
		return b, true
	}
	return a, true
}
