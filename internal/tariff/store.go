package tariff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	List(context.Context, string, string) ([]Tariff, error)
	Create(context.Context, string, string, Create, string, []byte) (Tariff, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

const selectTariff = `SELECT id,home_id,revision,fuel,rate::text,rate_unit,currency,rate_kind,source_ref,
  to_char(source_as_of AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),is_fallback,fixed_charge::text,
  to_char(effective_from AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
  CASE WHEN effective_to IS NULL THEN NULL ELSE to_char(effective_to AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"') END
  FROM tariff_revisions`

func scanTariff(row pgx.Row) (Tariff, error) {
	var t Tariff
	var fallback bool
	err := row.Scan(&t.ID, &t.HomeID, &t.Revision, &t.Fuel, &t.Rate, &t.RateUnit, &t.Currency, &t.RateKind, &t.SourceRef, &t.SourceAsOf, &fallback, &t.FixedCharge, &t.EffectiveFrom, &t.EffectiveTo)
	t.IsFallback = &fallback
	return t, err
}

func (s PGStore) List(ctx context.Context, owner, home string) ([]Tariff, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, selectTariff+` WHERE id IN (
        SELECT DISTINCT ON (fuel) id FROM tariff_revisions
        WHERE home_id=$1 AND effective_from <= now() AND (effective_to IS NULL OR effective_to > now())
          AND source_as_of >= now() - interval '18 months'
        ORDER BY fuel, CASE rate_kind WHEN 'user_tariff' THEN 0 WHEN 'official_tariff' THEN 1 ELSE 2 END,
                 effective_from DESC, revision DESC, id DESC
      ) ORDER BY fuel`, home)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tariff{}
	for rows.Next() {
		t, err := scanTariff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s PGStore) Create(ctx context.Context, owner, home string, in Create, key string, hash []byte) (Tariff, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Tariff{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tariff{}, ErrNotFound
	}
	if err != nil {
		return Tariff{}, err
	}
	var priorHash, priorBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_json FROM tariff_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND key=$3`, owner, home, key).Scan(&priorHash, &priorBody)
	if err == nil {
		if !bytes.Equal(priorHash, hash) {
			return Tariff{}, ErrKeyReuse
		}
		var prior Tariff
		if err = json.Unmarshal(priorBody, &prior); err != nil {
			return Tariff{}, err
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Tariff{}, err
	}
	var revision int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM tariff_revisions WHERE home_id=$1 AND fuel=$2`, home, in.Fuel).Scan(&revision)
	if err != nil {
		return Tariff{}, err
	}
	var t Tariff
	t.Create = in
	err = tx.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,fixed_charge,effective_from,effective_to)
      VALUES($1,$2,$3,$4::numeric,$5,$6,$7,$8,$9,$10,$11::numeric,$12,$13) RETURNING id`, home, revision, in.Fuel, in.Rate, in.RateUnit, in.Currency, in.RateKind, in.SourceRef, in.SourceAsOf, *in.IsFallback, in.FixedCharge, in.EffectiveFrom, in.EffectiveTo).Scan(&t.ID)
	if err != nil {
		return Tariff{}, err
	}
	t.HomeID, t.Revision = home, revision
	body, err := json.Marshal(t)
	if err != nil {
		return Tariff{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO tariff_idempotency(owner_user_id,home_id,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5)`, owner, home, key, hash, body)
	if err != nil {
		return Tariff{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Tariff{}, err
	}
	return t, nil
}
