package weather

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGLocations maps homes to shared weather_locations rows and stores their
// forecast days and alerts. Rows carry no home or owner id.
type PGLocations struct{ Pool *pgxpool.Pool }

type weatherLocation struct {
	Key, Country, PostalArea, Timezone, Provider string
	Latitude, Longitude                          float64
	GridOffice                                   *string
	GridX, GridY                                 *int
	Station, SiteCode                            *string
	ForecastExpiresAt, AlertsExpiresAt           *time.Time
	PointsExpiresAt                              *time.Time
}

var (
	zip5Pattern = regexp.MustCompile(`^[0-9]{5}(-[0-9]{4})?$`)
	caPostal    = regexp.MustCompile(`^[A-Z][0-9][A-Z][0-9][A-Z][0-9]$`)
)

// PostalArea returns the ZIP5 (US) or FSA (CA) used as the weather location.
func PostalArea(country, postal string) (string, bool) {
	postal = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(postal), " ", ""))
	switch strings.ToUpper(country) {
	case "US":
		if zip5Pattern.MatchString(postal) {
			return postal[:5], true
		}
	case "CA":
		if caPostal.MatchString(postal) {
			return postal[:3], true
		}
	}
	return "", false
}

// Resolve finds the home's location from its latest location revision and
// timezone and ensures the shared row exists; false when no centroid is known.
func (s PGLocations) Resolve(ctx context.Context, owner, home string) (weatherLocation, bool, error) {
	var country, postal, tz string
	err := s.Pool.QueryRow(ctx, `SELECT h.timezone,l.country_code,l.postal_code
FROM homes h JOIN LATERAL (SELECT country_code,postal_code FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l ON true
WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner).Scan(&tz, &country, &postal)
	if errors.Is(err, pgx.ErrNoRows) {
		return weatherLocation{}, false, ErrHomeNotFound
	}
	if err != nil {
		return weatherLocation{}, false, err
	}
	country = strings.ToUpper(country)
	area, ok := PostalArea(country, postal)
	if _, tzErr := time.LoadLocation(tz); !ok || tzErr != nil || tz == "Local" || len(tz) <= 2 {
		return weatherLocation{}, false, nil
	}
	var lat, lon float64
	err = s.Pool.QueryRow(ctx, `SELECT latitude::float8,longitude::float8 FROM weather_postal_centroids WHERE country=$1 AND postal_area=$2`, country, area).Scan(&lat, &lon)
	if errors.Is(err, pgx.ErrNoRows) {
		return weatherLocation{}, false, nil
	}
	if err != nil {
		return weatherLocation{}, false, err
	}
	provider := ProviderNWS
	if country == "CA" {
		provider = ProviderECCC
	}
	key := country + ":" + area + ":" + tz
	// A reloaded centroid invalidates the provider lookups and cached expiries.
	if _, err = s.Pool.Exec(ctx, `INSERT INTO weather_locations(location_key,country,postal_area,timezone,latitude,longitude,provider)
VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (location_key) DO UPDATE SET latitude=EXCLUDED.latitude,longitude=EXCLUDED.longitude,grid_office=NULL,grid_x=NULL,grid_y=NULL,
 observation_station=NULL,eccc_site_code=NULL,eccc_climate_station=NULL,points_expires_at=NULL,forecast_expires_at=NULL,alerts_expires_at=NULL,updated_at=now()
WHERE weather_locations.latitude<>EXCLUDED.latitude OR weather_locations.longitude<>EXCLUDED.longitude`, key, country, area, tz, lat, lon, provider); err != nil {
		return weatherLocation{}, false, err
	}
	loc, err := s.location(ctx, key)
	return loc, err == nil, err
}

func (s PGLocations) location(ctx context.Context, key string) (weatherLocation, error) {
	var l weatherLocation
	err := s.Pool.QueryRow(ctx, `SELECT location_key,country,postal_area,timezone,provider,latitude::float8,longitude::float8,
 grid_office,grid_x,grid_y,observation_station,eccc_site_code,forecast_expires_at,alerts_expires_at,points_expires_at
FROM weather_locations WHERE location_key=$1`, key).Scan(&l.Key, &l.Country, &l.PostalArea, &l.Timezone, &l.Provider, &l.Latitude, &l.Longitude,
		&l.GridOffice, &l.GridX, &l.GridY, &l.Station, &l.SiteCode, &l.ForecastExpiresAt, &l.AlertsExpiresAt, &l.PointsExpiresAt)
	return l, err
}

func (s PGLocations) savePoint(ctx context.Context, key string, pt nwsPoint, expires *time.Time) error {
	var station *string
	if pt.Station != "" {
		station = &pt.Station
	}
	_, err := s.Pool.Exec(ctx, `UPDATE weather_locations SET grid_office=$2,grid_x=$3,grid_y=$4,observation_station=$5,points_expires_at=$6,updated_at=now() WHERE location_key=$1`,
		key, pt.Office, pt.X, pt.Y, station, expires)
	return err
}

func (s PGLocations) saveSite(ctx context.Context, key, site string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE weather_locations SET eccc_site_code=$2,updated_at=now() WHERE location_key=$1`, key, site)
	return err
}

// saveForecast upserts days from today on. Observed rows are never touched and
// unchanged forecast rows are skipped, so revision moves only on a real change.
func (s PGLocations) saveForecast(ctx context.Context, key, provider, ref string, days []forecastDay, today string, now time.Time, expires *time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, d := range days {
			if d.Date < today {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO weather_days(location_key,local_date,high_f,low_f,source,provider,source_ref,retrieved_at)
VALUES($1,$2,$3,$4,'forecast',$5,$6,$7)
ON CONFLICT (location_key,local_date) DO UPDATE SET high_f=COALESCE(EXCLUDED.high_f,weather_days.high_f),low_f=COALESCE(EXCLUDED.low_f,weather_days.low_f),
 provider=EXCLUDED.provider,source_ref=EXCLUDED.source_ref,retrieved_at=EXCLUDED.retrieved_at,revision=weather_days.revision+1
WHERE weather_days.source='forecast' AND (weather_days.high_f IS DISTINCT FROM COALESCE(EXCLUDED.high_f,weather_days.high_f)
 OR weather_days.low_f IS DISTINCT FROM COALESCE(EXCLUDED.low_f,weather_days.low_f) OR weather_days.provider<>EXCLUDED.provider)`,
				key, d.Date, d.HighF, d.LowF, provider, ref, now); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE weather_locations SET forecast_retrieved_at=$2,forecast_expires_at=$3,updated_at=now() WHERE location_key=$1`, key, now, expires)
		return err
	})
}

// saveAlerts stamps every alert in this fetch with retrieved_at = alerts_retrieved_at,
// which marks the set returned by activeAlerts.
func (s PGLocations) saveAlerts(ctx context.Context, key, provider string, alerts []alertRecord, now time.Time, expires *time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, a := range alerts {
			if _, err := tx.Exec(ctx, `INSERT INTO weather_alerts(location_key,provider,source_alert_id,event,headline,starts_at,ends_at,source_url,retrieved_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (location_key,provider,source_alert_id) DO UPDATE SET event=EXCLUDED.event,headline=EXCLUDED.headline,starts_at=EXCLUDED.starts_at,
 ends_at=EXCLUDED.ends_at,source_url=EXCLUDED.source_url,retrieved_at=EXCLUDED.retrieved_at`,
				key, provider, a.SourceID, a.Event, a.Headline, a.StartsAt, a.EndsAt, a.SourceURL, now); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE weather_locations SET alerts_retrieved_at=$2,alerts_expires_at=$3,updated_at=now() WHERE location_key=$1`, key, now, expires)
		return err
	})
}

func (s PGLocations) activeAlerts(ctx context.Context, key string, now time.Time) ([]Alert, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.id,a.provider,a.event,a.headline,a.starts_at,a.ends_at,a.source_url
FROM weather_alerts a JOIN weather_locations l ON l.location_key=a.location_key
WHERE a.location_key=$1 AND a.retrieved_at=l.alerts_retrieved_at AND a.starts_at<=$2 AND a.ends_at>$2
ORDER BY a.starts_at,a.id`, key, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Provider, &a.Event, &a.Headline, &a.StartsAt, &a.EndsAt, &a.SourceURL); err != nil {
			return nil, err
		}
		a.Kind = AlertKind(a.Provider, a.Event)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s PGLocations) days(ctx context.Context, key, from string) ([]StoredDay, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,revision,local_date::text,source,high_f::float8,low_f::float8
FROM weather_days WHERE location_key=$1 AND local_date>=$2 ORDER BY local_date`, key, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StoredDay{}
	for rows.Next() {
		var d StoredDay
		if err := rows.Scan(&d.ID, &d.Revision, &d.Date, &d.Source, &d.HighF, &d.LowF); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AlertKind classifies an official alert event as hot, cold or neither.
func AlertKind(provider, event string) string {
	switch provider {
	case ProviderNWS:
		return nwsAlertKinds[event]
	case ProviderECCC:
		return ecccAlertKinds[strings.ToLower(strings.TrimSpace(event))]
	}
	return ""
}
