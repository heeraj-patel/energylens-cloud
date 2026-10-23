-- Official weather for 4.10 tips. Centroids are loaded by Datawiz; locations, forecast days and alerts
-- are written by Cloud; Datawiz fills observed days. No home or owner columns, so account purge is unchanged.
CREATE TABLE weather_postal_centroids (
  country     text NOT NULL CHECK (country IN ('US','CA')),
  postal_area text NOT NULL,
  latitude    numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
  longitude   numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
  source_url  text NOT NULL CHECK (source_url ~ '^https://'),
  edition     text NOT NULL CHECK (length(btrim(edition)) BETWEEN 1 AND 80),
  loaded_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (country, postal_area),
  CHECK ((country = 'US' AND postal_area ~ '^[0-9]{5}$') OR (country = 'CA' AND postal_area ~ '^[A-Z][0-9][A-Z]$'))
);
CREATE TABLE weather_locations (
  location_key          text PRIMARY KEY,
  country               text NOT NULL CHECK (country IN ('US','CA')),
  postal_area           text NOT NULL,
  timezone              text NOT NULL CHECK (length(timezone) > 2),
  latitude              numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
  longitude             numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
  provider              text NOT NULL CHECK (provider IN ('nws','eccc')),
  grid_office           text,
  grid_x                integer CHECK (grid_x >= 0),
  grid_y                integer CHECK (grid_y >= 0),
  observation_station   text,
  eccc_site_code        text,
  eccc_climate_station  text,
  forecast_retrieved_at timestamptz,
  forecast_expires_at   timestamptz,
  alerts_retrieved_at   timestamptz,
  alerts_expires_at     timestamptz,
  points_expires_at     timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CHECK (location_key = country || ':' || postal_area || ':' || timezone),
  CHECK ((country = 'US' AND provider = 'nws' AND postal_area ~ '^[0-9]{5}$') OR (country = 'CA' AND provider = 'eccc' AND postal_area ~ '^[A-Z][0-9][A-Z]$')),
  CHECK ((grid_office IS NULL) = (grid_x IS NULL) AND (grid_x IS NULL) = (grid_y IS NULL)),
  CHECK (provider = 'nws' OR (grid_office IS NULL AND observation_station IS NULL)),
  CHECK (provider = 'eccc' OR (eccc_site_code IS NULL AND eccc_climate_station IS NULL))
);
CREATE TABLE weather_days (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  revision     integer NOT NULL DEFAULT 1 CHECK (revision > 0),
  location_key text NOT NULL REFERENCES weather_locations(location_key),
  local_date   date NOT NULL,
  high_f       numeric(6,2) CHECK (high_f BETWEEN -100 AND 150),
  low_f        numeric(6,2) CHECK (low_f BETWEEN -100 AND 150),
  source       text NOT NULL CHECK (source IN ('forecast','observed')),
  provider     text NOT NULL CHECK (provider IN ('nws','eccc')),
  source_ref   text NOT NULL CHECK (length(btrim(source_ref)) BETWEEN 1 AND 500),
  retrieved_at timestamptz NOT NULL,
  UNIQUE (location_key, local_date),
  CHECK (high_f IS NOT NULL OR low_f IS NOT NULL)
);
-- Evidence refs cite id + revision: every update bumps revision by one, and observed is never replaced by forecast.
CREATE FUNCTION guard_weather_day_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.location_key <> OLD.location_key OR NEW.local_date <> OLD.local_date THEN
    RAISE EXCEPTION 'weather day identity is immutable';
  END IF;
  IF OLD.source = 'observed' AND NEW.source = 'forecast' THEN
    RAISE EXCEPTION 'observed weather days are never replaced by forecast';
  END IF;
  IF NEW.revision <> OLD.revision + 1 THEN
    RAISE EXCEPTION 'weather day revision must increase by one';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER weather_day_guard BEFORE UPDATE ON weather_days
  FOR EACH ROW EXECUTE FUNCTION guard_weather_day_change();
CREATE TABLE weather_alerts (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  location_key    text NOT NULL REFERENCES weather_locations(location_key),
  provider        text NOT NULL CHECK (provider IN ('nws','eccc')),
  source_alert_id text NOT NULL CHECK (length(btrim(source_alert_id)) BETWEEN 1 AND 500),
  event           text NOT NULL CHECK (length(btrim(event)) > 0),
  headline        text,
  starts_at       timestamptz NOT NULL,
  ends_at         timestamptz NOT NULL,
  source_url      text NOT NULL CHECK (source_url ~ '^https://'),
  retrieved_at    timestamptz NOT NULL,
  UNIQUE (location_key, provider, source_alert_id),
  CHECK (ends_at > starts_at)
);
CREATE INDEX weather_alerts_location_ends_idx ON weather_alerts(location_key, ends_at);
