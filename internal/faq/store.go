package faq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"energylens.local/cloud/internal/weather"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home or appliance not found")

type CurrentEstimate struct {
	Result json.RawMessage
	AsOf   time.Time
}

type EvidenceStore interface {
	CurrentEstimate(context.Context, string, string, *string, time.Time) (*CurrentEstimate, error)
	CurrentWeather(context.Context, string, string, time.Time) ([]weather.Observation, int64, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

// CurrentRecentUse reads the latest FAQ answer for each current appliance.
// A later unknown answer intentionally hides a prior confirmed value.
func (s PGStore) CurrentRecentUse(ctx context.Context, owner, home string, appliance *string) ([]RecentUse, error) {
	rows, err := s.Pool.Query(ctx, `SELECT q.id,a.id,a.category,a.version,q.question_id,q.question_version,q.revision,q.status,q.answer_value,q.confirmed_at,q.recorded_at
FROM homes h JOIN appliances a ON a.home_id=h.id AND a.archived_at IS NULL
JOIN LATERAL (SELECT DISTINCT ON (question_id) id,question_id,question_version,revision,status,answer_value,confirmed_at,recorded_at
 FROM question_answers WHERE home_id=h.id AND appliance_id=a.id AND appliance_version=a.version AND question_id LIKE 'faq-contributors/v1.%'
 ORDER BY question_id,revision DESC) q ON true
WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL AND ($3::uuid IS NULL OR a.id=$3)`, home, owner, appliance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentUse
	for rows.Next() {
		var a RecentUse
		var confirmed *time.Time
		if err := rows.Scan(&a.ID, &a.ApplianceID, &a.Category, &a.ApplianceVersion, &a.QuestionID, &a.QuestionVersion, &a.Revision, &a.Status, &a.Value, &confirmed, &a.RecordedAt); err != nil {
			return nil, err
		}
		if confirmed != nil {
			a.ConfirmedAt = *confirmed
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CurrentWeather reads only the current owner's coarse location and the cached
// completed days. The assembler checks full coverage, freshness and source
// consistency; this read never calls a weather provider.
func (s PGStore) CurrentWeather(ctx context.Context, owner, home string, now time.Time) ([]weather.Observation, int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var timezone, country, postal string
	var locationRevision int64
	err = tx.QueryRow(ctx, `SELECT h.timezone,COALESCE(l.country_code,''),COALESCE(l.postal_code,''),COALESCE(l.revision,0)
FROM homes h LEFT JOIN LATERAL (SELECT country_code,postal_code,revision FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l ON true
WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL FOR SHARE OF h`, home, owner).Scan(&timezone, &country, &postal, &locationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	cell, ok := weather.CoarseCell(country, postal, timezone)
	if !ok {
		return nil, 0, tx.Commit(ctx)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return nil, 0, fmt.Errorf("invalid home timezone %q", timezone)
	}
	localNow := now.In(loc)
	today := fmt.Sprintf("%04d-%02d-%02d", localNow.Year(), localNow.Month(), localNow.Day())
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (provider,local_date) id,provider,cell_key,local_date::text,timezone,mean_temperature_c,source_revision,retrieved_at,expires_at
FROM weather_observations WHERE cell_key=$1 AND timezone=$2 AND local_date >= $3::date - 14 AND local_date < $3::date
ORDER BY provider,local_date,retrieved_at DESC,id DESC`, cell, timezone, today)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var observations []weather.Observation
	for rows.Next() {
		var o weather.Observation
		if err = rows.Scan(&o.ID, &o.Provider, &o.CellKey, &o.LocalDate, &o.Timezone, &o.MeanTemperatureC, &o.SourceRevision, &o.RetrievedAt, &o.ExpiresAt); err != nil {
			return nil, 0, err
		}
		observations = append(observations, o)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	return observations, locationRevision, tx.Commit(ctx)
}

// CurrentEstimate checks ownership even for questions that currently have no
// supported findings. A home row lock keeps ownership stable through the read.
func (s PGStore) CurrentEstimate(ctx context.Context, owner, home string, appliance *string, now time.Time) (*CurrentEstimate, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var timezone string
	err = tx.QueryRow(ctx, `SELECT timezone FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR SHARE`, home, owner).Scan(&timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if appliance == nil {
		return nil, tx.Commit(ctx)
	}
	var version int64
	err = tx.QueryRow(ctx, `SELECT version FROM appliances WHERE id=$1 AND home_id=$2 AND archived_at IS NULL FOR SHARE`, *appliance, home).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return nil, fmt.Errorf("invalid home timezone %q", timezone)
	}
	local := now.In(loc)
	start := fmt.Sprintf("%04d-%02d-01", local.Year(), local.Month())
	end := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc).Format("2006-01-02")
	var result CurrentEstimate
	err = tx.QueryRow(ctx, `SELECT e.result_json,e.as_of FROM estimate_runs e
 WHERE e.home_id=$1 AND e.appliance_id=$2 AND e.appliance_version=$3
 AND e.period_start=$4 AND e.period_end=$5
 AND NOT EXISTS (SELECT 1 FROM estimate_run_invalidations i WHERE i.run_id=e.id)
 ORDER BY e.as_of DESC,e.id DESC LIMIT 1`, home, *appliance, version, start, end).Scan(&result.Result, &result.AsOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	return &result, tx.Commit(ctx)
}
