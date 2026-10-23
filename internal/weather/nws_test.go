package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func tlsProvider(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, cacheControl, body string) {
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("Content-Type", "application/geo+json")
	fmt.Fprint(w, body)
}

func TestNWSPointForecastAndAlerts(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	chicago, _ := time.LoadLocation("America/Chicago")
	var agents []string
	var srv *httptest.Server
	srv = tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.UserAgent())
		switch r.URL.Path {
		case "/points/30.37,-97.89":
			writeJSON(w, "public, max-age=86400", `{"properties":{"gridId":"EWX","gridX":150,"gridY":95,"observationStations":"`+srv.URL+`/gridpoints/EWX/150,95/stations"}}`)
		case "/gridpoints/EWX/150,95/stations":
			writeJSON(w, "public, max-age=86400, s-maxage=3600", `{"features":[{"properties":{"stationIdentifier":"KATT"}},{"properties":{"stationIdentifier":"KRYW"}}]}`)
		case "/gridpoints/EWX/150,95/forecast":
			writeJSON(w, "public, max-age=60, s-maxage=3600", `{"properties":{"periods":[
{"startTime":"2026-07-15T11:00:00-05:00","endTime":"2026-07-15T18:00:00-05:00","isDaytime":true,"temperature":101,"temperatureUnit":"F"},
{"startTime":"2026-07-15T18:00:00-05:00","endTime":"2026-07-16T06:00:00-05:00","isDaytime":false,"temperature":80,"temperatureUnit":"F"},
{"startTime":"2026-07-16T06:00:00-05:00","endTime":"2026-07-16T18:00:00-05:00","isDaytime":true,"temperature":39,"temperatureUnit":"C"}]}}`)
		case "/alerts/active":
			if r.URL.Query().Get("point") != "30.37,-97.89" {
				t.Errorf("alert point %q", r.URL.RawQuery)
			}
			writeJSON(w, "public, max-age=5, s-maxage=5", `{"features":[
{"id":"https://x/a1","properties":{"id":"urn:a1","@id":"https://api.weather.gov/alerts/urn:a1","event":"Extreme Heat Warning","headline":"Extreme Heat Warning until Thursday","status":"Actual","messageType":"Alert","effective":"2026-07-15T10:00:00-05:00","onset":"2026-07-15T12:00:00-05:00","expires":"2026-07-16T20:00:00-05:00","ends":null}},
{"id":"https://x/a2","properties":{"id":"urn:a2","@id":"https://api.weather.gov/alerts/urn:a2","event":"Heat Advisory","status":"Actual","messageType":"Cancel","effective":"2026-07-15T10:00:00-05:00","expires":"2026-07-15T20:00:00-05:00"}},
{"id":"https://x/a3","properties":{"id":"urn:a3","@id":"https://api.weather.gov/alerts/urn:a3","event":"Air Quality Alert","headline":"","status":"Actual","messageType":"Update","effective":"2026-07-15T10:00:00-05:00","expires":"2026-07-15T20:00:00-05:00","ends":"2026-07-16T00:00:00-05:00"}},
{"id":"https://x/a4","properties":{"id":"urn:a4","event":"Freeze Warning","status":"Test","messageType":"Alert","effective":"2026-07-15T10:00:00-05:00","expires":"2026-07-15T20:00:00-05:00"}}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := NWS{BaseURL: srv.URL, UserAgent: "EnergyLens test", Client: srv.Client()}
	ctx := context.Background()
	pt, exp, ok, err := p.point(ctx, 30.37, -97.89, now)
	if err != nil || pt != (nwsPoint{Office: "EWX", X: 150, Y: 95, Station: "KATT"}) || !ok || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("point: %+v %v %t %v", pt, exp, ok, err)
	}
	days, ref, exp, ok, err := p.forecast(ctx, pt, chicago, now)
	if err != nil || !ok || !exp.Equal(now.Add(time.Hour)) || ref != srv.URL+"/gridpoints/EWX/150,95/forecast" {
		t.Fatalf("forecast: %v %t %q %v", exp, ok, ref, err)
	}
	if len(days) != 2 || days[0].Date != "2026-07-15" || *days[0].HighF != 101 || days[0].LowF != nil ||
		days[1].Date != "2026-07-16" || *days[1].LowF != 80 || *days[1].HighF != 102.2 {
		t.Fatalf("forecast days: %+v", days)
	}
	alerts, exp, ok, err := p.alerts(ctx, 30.37, -97.89, now)
	if err != nil || !ok || !exp.Equal(now.Add(5*time.Second)) || len(alerts) != 2 {
		t.Fatalf("alerts: %+v %v %t %v", alerts, exp, ok, err)
	}
	heat := alerts[0]
	if heat.SourceID != "urn:a1" || heat.Kind != KindHot || !heat.StartsAt.Equal(time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)) ||
		!heat.EndsAt.Equal(time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)) || heat.Headline == nil || heat.SourceURL != "https://api.weather.gov/alerts/urn:a1" {
		t.Fatalf("heat alert (ends from expires): %+v", heat)
	}
	if alerts[1].Kind != "" || alerts[1].Headline != nil || !alerts[1].EndsAt.Equal(time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("other alert: %+v", alerts[1])
	}
	for _, a := range agents {
		if a != "EnergyLens test" {
			t.Fatalf("user agent %q", a)
		}
	}
}

func TestNWSPoint404IsNoProviderPoint(t *testing.T) {
	srv := tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"title":"Data Unavailable For Requested Point"}`)
	})
	_, _, _, err := NWS{BaseURL: srv.URL, Client: srv.Client()}.point(context.Background(), 18.2, -66.5, time.Now())
	if !errors.Is(err, ErrNoProviderPoint) {
		t.Fatalf("404 point: %v", err)
	}
}

func TestNWSTimeoutAndServerErrorAreUnavailable(t *testing.T) {
	srv := tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alerts/active" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	p := NWS{BaseURL: srv.URL, Client: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := p.point(ctx, 30.37, -97.89, time.Now()); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	if _, _, _, err := p.alerts(context.Background(), 30.37, -97.89, time.Now()); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("503: %v", err)
	}
}

func TestNWSBadBodiesAreInvalid(t *testing.T) {
	srv := tlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/points/30.37,-97.89":
			writeJSON(w, "", `{"properties":{"gridId":"ewx","gridX":1,"gridY":2}}`)
		default:
			writeJSON(w, "", `{"properties":{"periods":[{"startTime":"2026-07-15T11:00:00-05:00","isDaytime":true,"temperature":90,"temperatureUnit":"K"}]}}`)
		}
	})
	p := NWS{BaseURL: srv.URL, Client: srv.Client()}
	if _, _, _, err := p.point(context.Background(), 30.37, -97.89, time.Now()); !errors.Is(err, ErrInvalidSourceData) {
		t.Fatalf("bad grid: %v", err)
	}
	if _, _, _, _, err := p.forecast(context.Background(), nwsPoint{Office: "EWX", X: 1, Y: 2}, time.UTC, time.Now()); !errors.Is(err, ErrInvalidSourceData) {
		t.Fatalf("bad unit: %v", err)
	}
}

func TestAlertKindUsesOfficialNames(t *testing.T) {
	cases := map[[2]string]string{
		{ProviderNWS, "Heat Advisory"}: KindHot, {ProviderNWS, "Extreme Heat Watch"}: KindHot, {ProviderNWS, "Extreme Heat Warning"}: KindHot,
		{ProviderNWS, "Cold Weather Advisory"}: KindCold, {ProviderNWS, "Extreme Cold Watch"}: KindCold, {ProviderNWS, "Extreme Cold Warning"}: KindCold,
		{ProviderNWS, "Freeze Watch"}: KindCold, {ProviderNWS, "Freeze Warning"}: KindCold, {ProviderNWS, "Frost Advisory"}: "",
		{ProviderNWS, "heat advisory"}: "", {ProviderECCC, "heat warning"}: KindHot, {ProviderECCC, "Cold warning"}: KindCold,
		{ProviderECCC, "extreme cold warning"}: KindCold, {ProviderECCC, "frost advisory"}: "", {ProviderECCC, "Heat Advisory"}: "",
	}
	for in, want := range cases {
		if got := AlertKind(in[0], in[1]); got != want {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}
