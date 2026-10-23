package weather

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

const ecccTorontoForecast = `{"id":"on-143","properties":{"lastUpdated":"2026-07-15T15:10:00Z","identifier":"on-143",
"forecastGroup":{"timestamp":{"en":"2026-07-15T15:00:00Z","fr":"2026-07-15T15:00:00Z"},"forecasts":[
{"period":{"textForecastName":{"en":"Today"},"value":{"en":"Wednesday"}},"temperatures":{"temperature":[{"class":{"en":"high"},"units":{"en":"C"},"value":{"en":31,"fr":31}}]}},
{"period":{"textForecastName":{"en":"Tonight"},"value":{"en":"Wednesday night"}},"temperatures":{"temperature":[{"class":{"en":"low"},"units":{"en":"C"},"value":{"en":22,"fr":22}}]}},
{"period":{"textForecastName":{"en":"Thursday"},"value":{"en":"Thursday"}},"temperatures":{"temperature":[{"class":{"en":"high"},"units":{"en":"C"},"value":{"en":33,"fr":33}}]}},
{"period":{"textForecastName":{"en":"Thursday night"},"value":{"en":"Thursday night"}},"temperatures":{"temperature":[{"class":{"en":"low"},"units":{"en":"C"},"value":{"en":-3.5,"fr":-3.5}}]}}]}}}`

func TestECCCTorontoSiteForecastAndAlerts(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	toronto, _ := time.LoadLocation("America/Toronto")
	srv := tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections/citypageweather-realtime/items":
			if r.URL.Query().Get("bbox") != "-80.38,42.65,-78.38,44.65" {
				t.Errorf("site bbox %q", r.URL.Query().Get("bbox"))
			}
			writeJSON(w, "", `{"features":[{"id":"on-128","geometry":{"coordinates":[-79.39,43.63]}},{"id":"on-143","geometry":{"coordinates":[-79.38,43.65]}},{"id":"bad id","geometry":{"coordinates":[-79.38,43.65]}}]}`)
		case "/collections/citypageweather-realtime/items/on-143":
			writeJSON(w, "", ecccTorontoForecast)
		case "/collections/weather-alerts/items":
			writeJSON(w, "", `{"features":[
{"id":"al-1_z1","properties":{"alert_name_en":"heat warning","status_en":"issued","publication_datetime":"2026-07-15T11:00:00.000Z","validity_datetime":"2026-07-15T12:00:00.000Z","event_end_datetime":null,"expiration_datetime":"2026-07-16T03:00:00.000Z"}},
{"id":"al-1_z1","properties":{"alert_name_en":"heat warning","status_en":"issued","publication_datetime":"2026-07-15T11:00:00.000Z","expiration_datetime":"2026-07-16T03:00:00.000Z"}},
{"id":"al-2_z1","properties":{"alert_name_en":"special weather statement","status_en":"ended","publication_datetime":"2026-07-15T11:00:00.000Z","event_end_datetime":"2026-07-16T03:00:00.000Z"}},
{"id":"al-3_z1","properties":{"alert_name_en":"fog advisory","status_en":"continued","publication_datetime":"2026-07-15T11:00:00.000Z","event_end_datetime":"2026-07-15T20:00:00.000Z","expiration_datetime":"2026-07-15T19:00:00.000Z"}}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := ECCC{BaseURL: srv.URL, UserAgent: "EnergyLens test", Client: srv.Client()}
	ctx := context.Background()
	site, err := p.site(ctx, 43.65, -79.38)
	if err != nil || site != "on-143" {
		t.Fatalf("site: %q %v", site, err)
	}
	days, ref, _, cacheable, err := p.forecast(ctx, site, toronto, now)
	if err != nil || cacheable || ref != srv.URL+"/collections/citypageweather-realtime/items/on-143" {
		t.Fatalf("forecast: %q %t %v", ref, cacheable, err)
	}
	if len(days) != 3 || days[0].Date != "2026-07-15" || *days[0].HighF != 87.8 || days[0].LowF != nil ||
		days[1].Date != "2026-07-16" || *days[1].LowF != 71.6 || *days[1].HighF != 91.4 ||
		days[2].Date != "2026-07-17" || *days[2].LowF != 25.7 || days[2].HighF != nil {
		t.Fatalf("days: %+v", days)
	}
	alerts, _, cacheable, err := p.alerts(ctx, 43.65, -79.38, now)
	if err != nil || cacheable || len(alerts) != 2 {
		t.Fatalf("alerts: %+v %t %v", alerts, cacheable, err)
	}
	if a := alerts[0]; a.Kind != KindHot || a.Event != "heat warning" || !a.EndsAt.Equal(time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)) ||
		!a.StartsAt.Equal(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)) || a.SourceURL != srv.URL+"/collections/weather-alerts/items/al-1_z1" {
		t.Fatalf("heat warning (ends from expiry): %+v", a)
	}
	if a := alerts[1]; a.Kind != "" || !a.EndsAt.Equal(time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("fog advisory: %+v", a)
	}
}

func TestECCCMissingSiteAndBadPeriod(t *testing.T) {
	srv := tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections/citypageweather-realtime/items":
			writeJSON(w, "", `{"features":[]}`)
		default:
			writeJSON(w, "", `{"properties":{"forecastGroup":{"timestamp":{"en":"2026-07-15T15:00:00Z"},"forecasts":[{"period":{"value":{"en":"Someday"}}}]}}}`)
		}
	})
	p := ECCC{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := p.site(context.Background(), 60, -100); !errors.Is(err, ErrNoProviderPoint) {
		t.Fatalf("no site: %v", err)
	}
	if _, _, _, _, err := p.forecast(context.Background(), "on-1", time.UTC, time.Now()); !errors.Is(err, ErrInvalidSourceData) {
		t.Fatalf("bad period: %v", err)
	}
}
