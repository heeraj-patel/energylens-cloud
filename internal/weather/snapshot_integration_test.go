package weather

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeProviders struct {
	mu       sync.Mutex
	base     string
	calls    map[string]int
	nwsHigh  string
	nwsAlert string
}

func (f *fakeProviders) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *fakeProviders) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.URL.Path]++
	high, alert := f.nwsHigh, f.nwsAlert
	f.mu.Unlock()
	switch r.URL.Path {
	case "/points/30.37,-97.89":
		writeJSON(w, "public, max-age=86400", `{"properties":{"gridId":"EWX","gridX":150,"gridY":95,"observationStations":"`+f.base+`/gridpoints/EWX/150,95/stations"}}`)
	case "/gridpoints/EWX/150,95/stations":
		writeJSON(w, "public, max-age=86400, s-maxage=86400", `{"features":[{"properties":{"stationIdentifier":"KATT"}}]}`)
	case "/gridpoints/EWX/150,95/forecast":
		writeJSON(w, "public, max-age=60, s-maxage=3600", `{"properties":{"periods":[
{"startTime":"2026-07-15T11:00:00-05:00","endTime":"2026-07-15T18:00:00-05:00","isDaytime":true,"temperature":101,"temperatureUnit":"F"},
{"startTime":"2026-07-15T18:00:00-05:00","endTime":"2026-07-16T06:00:00-05:00","isDaytime":false,"temperature":80,"temperatureUnit":"F"},
{"startTime":"2026-07-16T06:00:00-05:00","endTime":"2026-07-16T18:00:00-05:00","isDaytime":true,"temperature":`+high+`,"temperatureUnit":"F"}]}}`)
	case "/alerts/active":
		writeJSON(w, "no-store", `{"features":[`+alert+`]}`)
	case "/points/30.27,-97.74":
		writeJSON(w, "max-age=600", `{"properties":{"gridId":"EWX","gridX":156,"gridY":91}}`)
	case "/gridpoints/EWX/156,91/forecast":
		time.Sleep(150 * time.Millisecond)
		writeJSON(w, "max-age=600", `{"properties":{"periods":[{"startTime":"2026-07-15T11:00:00-05:00","endTime":"2026-07-15T18:00:00-05:00","isDaytime":true,"temperature":95,"temperatureUnit":"F"}]}}`)
	case "/points/31.1,-97.1":
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	case "/collections/citypageweather-realtime/items":
		writeJSON(w, "", `{"features":[{"id":"on-128","geometry":{"coordinates":[-79.39,43.63]}},{"id":"on-143","geometry":{"coordinates":[-79.38,43.65]}}]}`)
	case "/collections/citypageweather-realtime/items/on-143":
		writeJSON(w, "", ecccTorontoForecast)
	case "/collections/weather-alerts/items":
		writeJSON(w, "", `{"features":[{"id":"al-1_z1","properties":{"alert_name_en":"heat warning","status_en":"issued","validity_datetime":"2026-07-15T12:00:00.000Z","expiration_datetime":"2026-07-16T03:00:00.000Z"}}]}`)
	default:
		http.NotFound(w, r)
	}
}

const nwsHeatAlert = `{"id":"https://api.weather.gov/alerts/urn:h1","properties":{"id":"urn:h1","@id":"https://api.weather.gov/alerts/urn:h1","event":"Extreme Heat Warning","headline":"Extreme Heat Warning until 8 PM","status":"Actual","messageType":"Alert","effective":"2026-07-15T12:00:00-05:00","expires":"2026-07-15T14:00:00-05:00"}}`

func TestPGWeatherSnapshotLiveFetch(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "weather_live_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO weather_postal_centroids(country,postal_area,latitude,longitude,source_url,edition) VALUES
('US','78732',30.37,-97.89,'https://www.census.gov/gazetteer','2025'),('US','78701',30.27,-97.74,'https://www.census.gov/gazetteer','2025'),
('US','76501',31.1,-97.1,'https://www.census.gov/gazetteer','2025'),('US','00901',18.2,-66.5,'https://www.census.gov/gazetteer','2025'),
('CA','M5V',43.65,-79.38,'https://www.statcan.gc.ca','2021')`); err != nil {
		t.Fatal(err)
	}
	owner, other := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, owner, other); err != nil {
		t.Fatal(err)
	}
	newHome := func(country, postal, tz string) string {
		home := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home',$3)`, home, owner, tz); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,$2,$3,'postal_code','user_entered')`, home, country, postal); err != nil {
			t.Fatal(err)
		}
		return home
	}
	austin := newHome("US", "78732-1234", "America/Chicago")
	fake := &fakeProviders{calls: map[string]int{}, nwsHigh: "103", nwsAlert: nwsHeatAlert}
	srv := tlsProvider(t, fake.handler)
	fake.base = srv.URL
	live := &Live{Store: PGLocations{Pool: pool}, NWS: NWS{BaseURL: srv.URL, UserAgent: "EnergyLens test", Client: srv.Client()}, ECCC: ECCC{BaseURL: srv.URL, UserAgent: "EnergyLens test", Client: srv.Client()}, Budget: 2 * time.Second}
	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	key := "US:78732:America/Chicago"

	if _, err = live.Snapshot(ctx, other, austin, now); !errors.Is(err, ErrHomeNotFound) {
		t.Fatalf("cross-owner snapshot: %v", err)
	}
	// Observed rows: today (forecast must not replace it) and an 11-day hot run ending yesterday.
	if _, err = pool.Exec(ctx, `INSERT INTO weather_locations(location_key,country,postal_area,timezone,latitude,longitude,provider) VALUES($1,'US','78732','America/Chicago',30.37,-97.89,'nws')`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO weather_days(location_key,local_date,high_f,low_f,source,provider,source_ref,retrieved_at)
SELECT $1,d::date,99.5,78,'observed','nws','KATT',$2 FROM generate_series('2026-07-04'::date,'2026-07-15'::date,'1 day') d`, key, now); err != nil {
		t.Fatal(err)
	}

	snap, err := live.Snapshot(ctx, owner, austin, now)
	if err != nil || snap.Status != StatusAvailable || snap.Provider != ProviderNWS || snap.LocationKey != key || snap.LastCompleted != "2026-07-14" {
		t.Fatalf("first snapshot: %+v %v", snap, err)
	}
	if !snap.Hot.Active || snap.Hot.Days != 11 || snap.Hot.LastDay != "2026-07-14" || snap.Cold.Active {
		t.Fatalf("runs: %+v %+v", snap.Hot, snap.Cold)
	}
	if len(snap.Alerts) != 1 || snap.Alerts[0].Kind != KindHot || snap.Alerts[0].Event != "Extreme Heat Warning" || !snap.Alerts[0].EndsAt.Equal(time.Date(2026, 7, 15, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("alerts: %+v", snap.Alerts)
	}
	type row struct {
		source   string
		high     *float64
		low      *float64
		revision int
	}
	readDay := func(date string) row {
		var r row
		if err := pool.QueryRow(ctx, `SELECT source,high_f::float8,low_f::float8,revision FROM weather_days WHERE location_key=$1 AND local_date=$2`, key, date).Scan(&r.source, &r.high, &r.low, &r.revision); err != nil {
			t.Fatalf("day %s: %v", date, err)
		}
		return r
	}
	if r := readDay("2026-07-15"); r.source != "observed" || *r.high != 99.5 || r.revision != 1 {
		t.Fatalf("forecast replaced observed: %+v", r)
	}
	if r := readDay("2026-07-16"); r.source != "forecast" || *r.high != 103 || *r.low != 80 || r.revision != 1 {
		t.Fatalf("forecast day: %+v", r)
	}
	var station string
	var pointsExp, forecastExp *time.Time
	var alertsExp *time.Time
	if err = pool.QueryRow(ctx, `SELECT observation_station,points_expires_at,forecast_expires_at,alerts_expires_at FROM weather_locations WHERE location_key=$1`, key).Scan(&station, &pointsExp, &forecastExp, &alertsExp); err != nil {
		t.Fatal(err)
	}
	if station != "KATT" || pointsExp == nil || !pointsExp.Equal(now.Add(24*time.Hour)) || forecastExp == nil || !forecastExp.Equal(now.Add(time.Hour)) || alertsExp != nil {
		t.Fatalf("cache expiries (max-age, s-maxage, no-store): %s %v %v %v", station, pointsExp, forecastExp, alertsExp)
	}

	// Inside s-maxage: forecast and points reused, no-store alerts refetched; an alert gone from the feed is not active.
	fake.mu.Lock()
	fake.nwsAlert = ""
	fake.mu.Unlock()
	snap, err = live.Snapshot(ctx, owner, austin, now.Add(10*time.Minute))
	if err != nil || snap.Status != StatusAvailable || len(snap.Alerts) != 0 {
		t.Fatalf("second snapshot: %+v %v", snap, err)
	}
	if fake.count("/points/30.37,-97.89") != 1 || fake.count("/gridpoints/EWX/150,95/forecast") != 1 || fake.count("/alerts/active") != 2 {
		t.Fatalf("cache reuse: %v", fake.calls)
	}

	// After s-maxage: unchanged forecast keeps its revision, a changed day bumps it by one.
	fake.mu.Lock()
	fake.nwsAlert = nwsHeatAlert
	fake.mu.Unlock()
	snap, err = live.Snapshot(ctx, owner, austin, now.Add(2*time.Hour))
	if err != nil || snap.Status != StatusAvailable || fake.count("/gridpoints/EWX/150,95/forecast") != 2 {
		t.Fatalf("refetch: %+v %v %v", snap, err, fake.calls)
	}
	if len(snap.Alerts) != 0 {
		t.Fatalf("expired alert still active: %+v", snap.Alerts)
	}
	if r := readDay("2026-07-16"); r.revision != 1 {
		t.Fatalf("no-op forecast bumped revision: %+v", r)
	}
	fake.mu.Lock()
	fake.nwsHigh = "104"
	fake.mu.Unlock()
	if snap, err = live.Snapshot(ctx, owner, austin, now.Add(4*time.Hour)); err != nil || snap.Status != StatusAvailable {
		t.Fatalf("changed forecast: %+v %v", snap, err)
	}
	if r := readDay("2026-07-16"); r.revision != 2 || *r.high != 104 || *r.low != 80 {
		t.Fatalf("changed forecast day: %+v", r)
	}
	if r := readDay("2026-07-15"); r.source != "observed" || r.revision != 1 {
		t.Fatalf("observed day touched: %+v", r)
	}

	// One in-flight fetch per location.
	downtown := newHome("US", "78701", "America/Chicago")
	var wg sync.WaitGroup
	var available atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := live.Snapshot(ctx, owner, downtown, now)
			if err == nil && s.Status == StatusAvailable {
				available.Add(1)
			}
		}()
	}
	wg.Wait()
	if available.Load() != 5 || fake.count("/points/30.27,-97.74") != 1 || fake.count("/gridpoints/EWX/156,91/forecast") != 1 {
		t.Fatalf("singleflight: %d %v", available.Load(), fake.calls)
	}

	// Unsupported, 404 point and timeout.
	for _, c := range []struct {
		home, reason string
	}{
		{newHome("US", "10001", "America/New_York"), ReasonUnsupportedLocation},
		{newHome("GB", "SW1A1AA", "Europe/London"), ReasonUnsupportedLocation},
		{newHome("US", "00901", "America/Puerto_Rico"), ReasonUnsupportedLocation},
		{newHome("US", "76501", "America/Chicago"), ReasonProviderUnavailable},
	} {
		live.Budget = 200 * time.Millisecond
		snap, err = live.Snapshot(ctx, owner, c.home, now)
		if err != nil || snap.Status != StatusUnavailable || snap.Reason != c.reason || len(snap.Alerts) != 0 {
			t.Fatalf("want %s: %+v %v", c.reason, snap, err)
		}
	}
	live.Budget = 2 * time.Second

	// Toronto resolves by FSA to ECCC; ECCC sends no Cache-Control, so nothing is reused but the site code.
	toronto := newHome("CA", "M5V 3L9", "America/Toronto")
	for i := 0; i < 2; i++ {
		snap, err = live.Snapshot(ctx, owner, toronto, now.Add(time.Duration(i)*time.Minute))
		if err != nil || snap.Status != StatusAvailable || snap.Provider != ProviderECCC || snap.LocationKey != "CA:M5V:America/Toronto" {
			t.Fatalf("toronto: %+v %v", snap, err)
		}
		if len(snap.Alerts) != 1 || snap.Alerts[0].Kind != KindHot || snap.Alerts[0].Headline != nil || !snap.Alerts[0].EndsAt.Equal(time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)) {
			t.Fatalf("toronto alerts: %+v", snap.Alerts)
		}
	}
	if fake.count("/collections/citypageweather-realtime/items") != 1 || fake.count("/collections/citypageweather-realtime/items/on-143") != 2 || fake.count("/collections/weather-alerts/items") != 2 {
		t.Fatalf("eccc calls: %v", fake.calls)
	}
	var site string
	var high float64
	if err = pool.QueryRow(ctx, `SELECT l.eccc_site_code,d.high_f::float8 FROM weather_locations l JOIN weather_days d USING(location_key) WHERE l.location_key='CA:M5V:America/Toronto' AND d.local_date='2026-07-16'`).Scan(&site, &high); err != nil || site != "on-143" || high != 91.4 {
		t.Fatalf("toronto stored: %q %v %v", site, high, err)
	}
}
