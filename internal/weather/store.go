package weather

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) HomeLocation(ctx context.Context, owner, home string) (Location, bool, error) {
	var loc Location
	var country, postal string
	err := s.Pool.QueryRow(ctx, `SELECT h.version,h.timezone,l.revision,l.country_code,l.postal_code
FROM homes h JOIN LATERAL (SELECT revision,country_code,postal_code FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l ON true
WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner).Scan(&loc.HomeVersion, &loc.Timezone, &loc.LocationRevision, &country, &postal)
	if errors.Is(err, pgx.ErrNoRows) {
		return Location{}, false, ErrHomeNotFound
	}
	if err != nil {
		return Location{}, false, err
	}
	loc.CellKey, _ = CoarseCell(country, postal, loc.Timezone)
	return loc, loc.CellKey != "", nil
}

func (s PGStore) Latest(ctx context.Context, provider, cell, day, timezone string) (*Observation, error) {
	var obs Observation
	err := s.Pool.QueryRow(ctx, `SELECT id,provider,cell_key,local_date::text,timezone,mean_temperature_c,source_revision,retrieved_at,expires_at FROM weather_observations WHERE provider=$1 AND cell_key=$2 AND local_date=$3 AND timezone=$4 ORDER BY retrieved_at DESC,id DESC LIMIT 1`, provider, cell, day, timezone).Scan(&obs.ID, &obs.Provider, &obs.CellKey, &obs.LocalDate, &obs.Timezone, &obs.MeanTemperatureC, &obs.SourceRevision, &obs.RetrievedAt, &obs.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &obs, nil
}

func (s PGStore) Save(ctx context.Context, obs Observation) (Observation, error) {
	err := s.Pool.QueryRow(ctx, `INSERT INTO weather_observations(provider,cell_key,local_date,timezone,mean_temperature_c,source_revision,retrieved_at,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,mean_temperature_c`, obs.Provider, obs.CellKey, obs.LocalDate, obs.Timezone, obs.MeanTemperatureC, obs.SourceRevision, obs.RetrievedAt, obs.ExpiresAt).Scan(&obs.ID, &obs.MeanTemperatureC)
	return obs, err
}
